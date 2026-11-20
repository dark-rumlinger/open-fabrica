package lan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPingTimeout = 1000 * time.Millisecond
	maxPingTimeout     = 3000 * time.Millisecond
)

func isAllowedLANIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// Block cloud metadata / link-local unicast (e.g. 169.254.169.254, fe80::).
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback()
}

// ValidateLANHost checks that host is a local-network address (RFC 1918 / ULA,
// loopback, localhost, or a .local mDNS hostname) and never a public internet
// host or cloud metadata endpoint.
func ValidateLANHost(host string) error {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" {
		return errors.New("host is required")
	}
	if h == "metadata.google.internal" || h == "metadata.goog" {
		return fmt.Errorf("host %q is blocked", host)
	}
	if ip := net.ParseIP(h); ip != nil {
		if !isAllowedLANIP(ip) {
			return fmt.Errorf("host %q is not a private LAN or loopback IP", host)
		}
		return nil
	}
	if h == "localhost" || strings.HasSuffix(h, ".local") || !strings.Contains(h, ".") {
		return nil
	}
	return fmt.Errorf("host %q must be a private LAN IP, localhost, single-label LAN hostname, or .local mDNS name", host)
}

// TCPPing tests whether host:port accepts a TCP connection on the local network.
// Every resolved IP is verified against isAllowedLANIP before dialing so
// ctx.lan.ping cannot be used to scan public internet hosts or cloud metadata.
func TCPPing(ctx context.Context, host string, port int, timeout time.Duration) (reachable bool, latencyMS int64, err error) {
	if err := ValidateLANHost(host); err != nil {
		return false, 0, err
	}
	if port <= 0 || port > 65535 {
		return false, 0, fmt.Errorf("invalid port %d (must be 1..65535)", port)
	}
	if timeout <= 0 {
		timeout = defaultPingTimeout
	}
	if timeout > maxPingTimeout {
		timeout = maxPingTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Resolve first and verify every target IP is LAN/loopback (prevents DNS rebinding to public/metadata IPs).
	var targetIP net.IP
	if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
		targetIP = ip
	} else {
		ips, rerr := net.DefaultResolver.LookupIPAddr(dctx, strings.TrimSpace(host))
		if rerr != nil || len(ips) == 0 {
			return false, 0, nil
		}
		for _, ipAddr := range ips {
			if !isAllowedLANIP(ipAddr.IP) {
				return false, 0, fmt.Errorf("host %q resolved to non-LAN IP %s", host, ipAddr.IP.String())
			}
			if targetIP == nil || ipAddr.IP.To4() != nil {
				targetIP = ipAddr.IP
			}
		}
	}

	start := time.Now()
	dialer := &net.Dialer{Timeout: timeout}
	conn, derr := dialer.DialContext(dctx, "tcp", net.JoinHostPort(targetIP.String(), strconv.Itoa(port)))
	elapsed := time.Since(start).Milliseconds()
	if derr != nil {
		return false, elapsed, nil
	}
	_ = conn.Close()
	return true, elapsed, nil
}
