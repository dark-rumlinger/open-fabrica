// Package netguard is the SSRF defence for every outbound HTTP request made on a
// model's or a project's behalf: URL pre-flight validation and hardened clients that
// re-check redirects and pin resolved IPs at dial time. Shared by the tools, the
// trigger and job runners, webhooks, server functions, and the public server, so it
// lives here rather than inside any one of them.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// SSRF defenses for outbound HTTP: scheme/private-IP validation and the
// hardened clients the manage_http, feed, and agent-fetch paths dial through.

// ValidateExternalURL blocks SSRF by rejecting dangerous schemes and private IPs.
func ValidateExternalURL(rawURL string) error {
	return ValidateExternalURLAllowingHost(rawURL, "")
}

// ValidateExternalURLAllowingHost is ValidateExternalURL with one scoped
// exception: when allowHost is non-empty and case-insensitively equals the URL's
// hostname, resolution to a private/internal IP is permitted. This is the opt-in
// for reaching a single explicitly-registered LAN device (e.g. a local Home
// Assistant hub) without lifting the SSRF block for any other host. The scheme
// check (http/https only) still applies to the allowed host.
func ValidateExternalURLAllowingHost(rawURL, allowHost string) error {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (only http/https)", scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("empty hostname")
	}
	// Scoped opt-in: the one explicitly allowed host may resolve to a private IP.
	if allowHost != "" && strings.EqualFold(host, allowHost) {
		return nil
	}
	// Resolve the hostname to IPs and check each one.
	ips, err := net.LookupHost(host) // #nosec G704 -- this IS the SSRF guard; every resolved IP is rejected below when private/internal
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", host, err)
	}
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return fmt.Errorf("unparseable resolved IP %q", ipStr)
		}
		if IsPrivateIP(ip) {
			return fmt.Errorf("resolved to private/internal IP %s", ipStr)
		}
	}
	return nil
}

// cgnatBlock is the RFC 6598 carrier-grade NAT range (100.64.0.0/10). It is
// routable-looking space that net.IP.IsPrivate does not cover, yet it commonly
// fronts internal/cloud infrastructure, so it is treated as private for SSRF.
var cgnatBlock = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// nonPublicBlocks are ranges net.IP's helpers do not flag but that never hold a
// public host an external fetch should reach: "this network" (0.0.0.0/8), IETF
// protocol assignments, the benchmarking range, and the reserved class E space
// that also contains the limited broadcast address. The IPv6 entries are
// transition mechanisms that embed an IPv4 address in a form a plain IPv4 check
// would not see through (NAT64 64:ff9b::/96 and 6to4 2002::/16 are unwrapped
// separately), plus Teredo tunnels and the deprecated IPv4-compatible ::/96.
var nonPublicBlocks = mustCIDRs(
	"0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
	"2001::/32", "::/96",
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, len(cidrs))
	for i, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("netguard: bad CIDR " + c)
		}
		out[i] = n
	}
	return out
}

// embeddedIPv4 returns the IPv4 address a NAT64 (64:ff9b::/96) or 6to4 (2002::/16)
// IPv6 address wraps, or nil when ip is neither. A fetch to such an address reaches
// that IPv4 host through a gateway, so the wrapped address is what must be checked.
func embeddedIPv4(ip net.IP) net.IP {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return nil
	}
	switch {
	case v6[0] == 0x00 && v6[1] == 0x64 && v6[2] == 0xff && v6[3] == 0x9b && allZero(v6[4:12]):
		return net.IPv4(v6[12], v6[13], v6[14], v6[15])
	case v6[0] == 0x20 && v6[1] == 0x02:
		return net.IPv4(v6[2], v6[3], v6[4], v6[5])
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// IsPrivateIP reports whether ip is an address an external fetch must not reach:
// loopback, unspecified, link-local, multicast, RFC-1918 private, RFC-6598 CGNAT
// space, the non-public blocks above, or an IPv6 transition address wrapping one
// of those. IPv4-mapped IPv6 addresses (e.g. ::ffff:10.0.0.1) are unmapped to
// their 4-byte form first, so a mapped internal address cannot slip past the IPv4
// range checks.
func IsPrivateIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if cgnatBlock.Contains(ip) {
		return true
	}
	for _, n := range nonPublicBlocks {
		if n.Contains(ip) {
			return true
		}
	}
	if inner := embeddedIPv4(ip); inner != nil {
		return IsPrivateIP(inner)
	}
	return false
}

// SSRFSafeClient returns an http.Client hardened against SSRF for external
// URLs, for callers outside this package (e.g. the trigger runner) that fetch
// config-supplied URLs. Pair it with ValidateExternalURL for pre-flight checks.
func SSRFSafeClient() *http.Client {
	return clientForHost("")
}

// SSRFSafeClientAllowingHost is SSRFSafeClient with a scoped exception: the one
// host equal to allowHost may resolve to a private IP and still be dialed. Every
// other host is rejected at connect time, so DNS-rebinding to a different
// internal address stays blocked. Used by manage_providers for a provider
// explicitly registered with allow_private_host (a trusted LAN device).
func SSRFSafeClientAllowingHost(allowHost string) *http.Client {
	return clientForHost(allowHost)
}

// clientForHost builds the SSRF-hardened client. It re-validates redirect
// destinations and checks resolved IPs at connect time to prevent DNS rebinding.
// When allowHost is non-empty, that single host (matched exactly) is exempt from
// the private-IP block; all other hosts remain blocked.
func clientForHost(allowHost string) *http.Client {
	client := &http.Client{Timeout: 30 * time.Second}

	// Re-validate every redirect destination against SSRF rules.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return ValidateExternalURLAllowingHost(req.URL.String(), allowHost)
	}

	// Validate resolved IPs at connect time to prevent DNS rebinding.
	client.Transport = &http.Transport{
		// A server that accepts the connection and then stalls must not hold a worker.
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			hostAllowed := allowHost != "" && strings.EqualFold(host, allowHost)
			ips, err := net.DefaultResolver.LookupHost(ctx, host)
			if err != nil {
				return nil, err
			}
			if !hostAllowed {
				for _, ipStr := range ips {
					ip := net.ParseIP(ipStr)
					if ip != nil && IsPrivateIP(ip) {
						return nil, fmt.Errorf("blocked: resolved to private IP %s", ipStr)
					}
				}
			}
			// Dial the first resolved IP directly to pin the resolution.
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0], port))
		},
	}

	return client
}
