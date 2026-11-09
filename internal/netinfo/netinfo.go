// Package netinfo detects how this machine is reachable: its public internet
// IP and its local interface addresses. The admin hosting endpoints use it to
// render the exact DNS record and port-forward instructions instead of telling
// the user to "configure DNS".
//
// Nothing here asks a service on the internet. The public IP comes from the
// router (over UPnP, NAT-PMP or PCP, all LAN-only), from a public address on a
// local interface, or from the project's own domain as a last resort.
package netinfo

import (
	"context"
	"net"
	"sync"
	"time"

	"openfabrica/internal/portmap"
)

// Where Info.PublicIP came from.
const (
	SourceRouter    = "router"
	SourceInterface = "interface"
	SourceDNS       = "dns"
)

// Info describes this machine's network position. PublicIP is empty when
// detection failed (the UI renders a manual fallback). NATLikely is true when
// no public IPv4 is bound to a local interface, so inbound ports need
// forwarding. The router's own external address is only used as PublicIP
// when it really is public; otherwise one of two flags says why not:
//
//   - SharedAddress: the router holds a carrier-grade NAT address, shared with
//     other customers of the provider. No port forward anywhere in the home
//     can get past that.
//   - DoubleNAT: the router holds a private address, so it sits behind another
//     router (usually the provider's modem). That one needs the forward too,
//     or has to be switched to bridge mode.
type Info struct {
	PublicIP       string      `json:"public_ip,omitempty"`
	PublicIPSource string      `json:"public_ip_source,omitempty"`
	LocalIPs       []string    `json:"local_ips"`
	NATLikely      bool        `json:"nat_likely"`
	Router         *RouterInfo `json:"router,omitempty"`
	SharedAddress  bool        `json:"shared_address"`
	DoubleNAT      bool        `json:"double_nat"`
	IPv6           []string    `json:"ipv6"`
}

// RouterInfo describes the router that answered discovery. Every protocol
// portmap speaks can forward ports, so CanMap is true whenever one answered.
type RouterInfo struct {
	Protocol   string `json:"protocol"`
	ExternalIP string `json:"external_ip,omitempty"`
	CanMap     bool   `json:"can_map"`
}

// Detector resolves Info with a cached router lookup. Local interface
// addresses are read fresh on every call (cheap, and they change when the
// machine moves networks); router discovery is cached, a miss for less time
// than a hit so switching UPnP on in the router shows up quickly.
type Detector struct {
	probe  func(context.Context) (portmap.Router, error)
	lookup func(ctx context.Context, host string) ([]net.IP, error)
	locals func() []string

	hitTTL, missTTL time.Duration

	mu        sync.Mutex
	router    *portmap.Router
	fetchedAt time.Time
}

// NewDetector builds a Detector that asks the router on the local network.
func NewDetector() *Detector {
	return &Detector{
		probe: func(ctx context.Context) (portmap.Router, error) {
			_, r, err := portmap.Discover(ctx)
			return r, err
		},
		lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		locals:  localIPs,
		hitTTL:  10 * time.Minute,
		missTTL: time.Minute,
	}
}

// Detect returns the current network Info. Never returns an error: a failed
// public-IP lookup yields an empty PublicIP, which callers treat as
// "detection unavailable".
func (d *Detector) Detect(ctx context.Context) Info {
	return d.DetectFor(ctx, "")
}

// DetectFor is Detect with a project domain to fall back on: when neither the
// router nor an interface reveals the public IP, the address the domain
// currently points to is reported instead (source "dns"). That is only what
// the domain says, not proof it is this machine, and the UI words it that way.
func (d *Detector) DetectFor(ctx context.Context, domain string) Info {
	locals := d.locals()
	info := Info{LocalIPs: locals, IPv6: globalIPv6(locals), NATLikely: true}
	if info.LocalIPs == nil {
		info.LocalIPs = []string{}
	}

	if ip := publicIPv4(locals); ip != "" {
		info.PublicIP, info.PublicIPSource = ip, SourceInterface
		info.NATLikely = false
	}

	if r := d.routerCached(ctx); r != nil {
		info.Router = &RouterInfo{Protocol: r.Protocol, ExternalIP: r.ExternalIP, CanMap: true}
		if ext := net.ParseIP(r.ExternalIP); ext != nil && info.PublicIPSource == "" {
			switch {
			case portmap.IsCarrierNAT(ext):
				info.SharedAddress = true
			case ext.IsPrivate():
				info.DoubleNAT = true
			case isPublic(ext):
				info.PublicIP, info.PublicIPSource = r.ExternalIP, SourceRouter
			}
		}
	}

	if info.PublicIP == "" && domain != "" && net.ParseIP(domain) == nil {
		if ips, err := d.lookup(ctx, domain); err == nil {
			for _, ip := range ips {
				if ip4 := ip.To4(); ip4 != nil && isPublic(ip4) {
					info.PublicIP, info.PublicIPSource = ip4.String(), SourceDNS
					break
				}
			}
		}
	}
	return info
}

// routerCached returns the cached router, rediscovering it when the cache is
// stale. A failed discovery is cached too, so a machine with no router does
// not wait out a discovery on every status poll.
func (d *Detector) routerCached(ctx context.Context) *portmap.Router {
	d.mu.Lock()
	defer d.mu.Unlock()
	ttl := d.missTTL
	if d.router != nil {
		ttl = d.hitTTL
	}
	if !d.fetchedAt.IsZero() && time.Since(d.fetchedAt) < ttl {
		return d.router
	}
	d.router = nil
	if r, err := d.probe(ctx); err == nil {
		d.router = &r
	}
	d.fetchedAt = time.Now()
	return d.router
}

// Refresh drops the cached router so the next Detect rediscovers it.
func (d *Detector) Refresh() {
	d.mu.Lock()
	d.fetchedAt = time.Time{}
	d.mu.Unlock()
}

// isPublic reports whether ip is routable on the internet: not private,
// carrier-grade NAT, loopback, link-local, or otherwise special.
func isPublic(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !portmap.IsCarrierNAT(ip)
}

// publicIPv4 returns the first public IPv4 bound to a local interface: the
// case of a server with a public address and no router in front of it.
func publicIPv4(locals []string) string {
	for _, s := range locals {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil && isPublic(ip) {
			return s
		}
	}
	return ""
}

// globalIPv6 returns the internet-routable IPv6 addresses among locals.
// Unique-local (fc00::/7) addresses are private; IsPrivate covers them.
func globalIPv6(locals []string) []string {
	out := []string{}
	for _, s := range locals {
		if ip := net.ParseIP(s); ip != nil && ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			out = append(out, s)
		}
	}
	return out
}

// localIPs lists the machine's non-loopback, non-link-local interface
// addresses. These are what a router forwards to, so IPv4 comes first.
func localIPs() []string {
	var v4, v6 []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				v4 = append(v4, ip4.String())
			} else {
				v6 = append(v6, ip.String())
			}
		}
	}
	return append(v4, v6...)
}
