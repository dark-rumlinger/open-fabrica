package netinfo

import (
	"context"
	"net"
	"testing"
	"time"

	"openfabrica/internal/portmap"
)

// newTestDetector builds a Detector with an injected router, DNS and local
// addresses. router nil means no router answers.
func newTestDetector(router *portmap.Router, dns map[string][]net.IP, locals []string) (*Detector, *int) {
	probes := 0
	return &Detector{
		probe: func(context.Context) (portmap.Router, error) {
			probes++
			if router == nil {
				return portmap.Router{}, portmap.ErrNoRouter
			}
			return *router, nil
		},
		lookup: func(_ context.Context, host string) ([]net.IP, error) {
			if ips, ok := dns[host]; ok {
				return ips, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		},
		locals:  func() []string { return locals },
		hitTTL:  time.Minute,
		missTTL: time.Minute,
	}, &probes
}

func TestDetectPrefersRouter(t *testing.T) {
	d, _ := newTestDetector(
		&portmap.Router{Protocol: "upnp", ExternalIP: "203.0.113.7"},
		map[string][]net.IP{"example.com": {net.ParseIP("198.51.100.1")}},
		[]string{"192.168.1.20", "2001:db8:1::20", "fd00::1"},
	)
	info := d.DetectFor(context.Background(), "example.com")
	if info.PublicIP != "203.0.113.7" || info.PublicIPSource != SourceRouter {
		t.Errorf("public = %s (%s), want the router's address", info.PublicIP, info.PublicIPSource)
	}
	if !info.NATLikely || info.SharedAddress {
		t.Errorf("nat_likely = %v, shared = %v", info.NATLikely, info.SharedAddress)
	}
	if info.Router == nil || !info.Router.CanMap || info.Router.Protocol != "upnp" {
		t.Errorf("router = %+v", info.Router)
	}
	// 2001:db8::/32 is documentation space but still global unicast by Go's
	// definition; fd00::/8 is unique-local and must be dropped.
	if len(info.IPv6) != 1 || info.IPv6[0] != "2001:db8:1::20" {
		t.Errorf("ipv6 = %v", info.IPv6)
	}
}

func TestDetectPublicInterface(t *testing.T) {
	d, _ := newTestDetector(nil, nil, []string{"10.0.0.5", "203.0.113.50"})
	info := d.Detect(context.Background())
	if info.PublicIP != "203.0.113.50" || info.PublicIPSource != SourceInterface || info.NATLikely {
		t.Errorf("info = %+v, want the interface address with no NAT", info)
	}
}

func TestDetectFallsBackToDomain(t *testing.T) {
	d, _ := newTestDetector(nil,
		map[string][]net.IP{"example.com": {net.ParseIP("10.1.1.1"), net.ParseIP("198.51.100.1")}},
		[]string{"192.168.1.20"})
	info := d.DetectFor(context.Background(), "example.com")
	if info.PublicIP != "198.51.100.1" || info.PublicIPSource != SourceDNS {
		t.Errorf("public = %s (%s), want the domain's public A record", info.PublicIP, info.PublicIPSource)
	}
	if info := d.Detect(context.Background()); info.PublicIP != "" {
		t.Errorf("public = %s with no router, interface, or domain", info.PublicIP)
	}
}

func TestDetectRouterBehindAnotherNAT(t *testing.T) {
	dns := map[string][]net.IP{"example.com": {net.ParseIP("198.51.100.1")}}
	for _, tc := range []struct {
		name, routerIP     string
		wantShared, wantDN bool
	}{
		{"carrier-grade NAT", "100.78.241.44", true, false},
		{"double NAT", "192.168.1.20", false, true},
		{"public", "203.0.113.7", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDetector(&portmap.Router{Protocol: "upnp", ExternalIP: tc.routerIP}, dns, []string{"192.168.178.20"})
			info := d.DetectFor(context.Background(), "example.com")
			if info.SharedAddress != tc.wantShared || info.DoubleNAT != tc.wantDN {
				t.Errorf("shared = %v, double = %v; want %v, %v", info.SharedAddress, info.DoubleNAT, tc.wantShared, tc.wantDN)
			}
			if info.Router.ExternalIP != tc.routerIP {
				t.Errorf("router external_ip = %s, want it reported as-is", info.Router.ExternalIP)
			}
			// A non-public router address must never become the A record;
			// the domain fallback takes over instead.
			wantIP, wantSrc := "198.51.100.1", SourceDNS
			if !tc.wantShared && !tc.wantDN {
				wantIP, wantSrc = tc.routerIP, SourceRouter
			}
			if info.PublicIP != wantIP || info.PublicIPSource != wantSrc {
				t.Errorf("public = %s (%s), want %s (%s)", info.PublicIP, info.PublicIPSource, wantIP, wantSrc)
			}
		})
	}
}

func TestRouterMissIsCached(t *testing.T) {
	d, probes := newTestDetector(nil, nil, nil)
	d.Detect(context.Background())
	d.Detect(context.Background())
	if *probes != 1 {
		t.Errorf("router probed %d times, want 1 (a miss should be cached)", *probes)
	}
	d.Refresh()
	d.Detect(context.Background())
	if *probes != 2 {
		t.Errorf("router probed %d times after Refresh, want 2", *probes)
	}
}

func TestWatcherFlagsAfterTwoMismatches(t *testing.T) {
	dns := map[string][]net.IP{
		"moved.example":   {net.ParseIP("198.51.100.1")},
		"correct.example": {net.ParseIP("203.0.113.7")},
	}
	d, _ := newTestDetector(&portmap.Router{Protocol: "upnp", ExternalIP: "203.0.113.7"}, dns, nil)
	w := NewWatcher(d, func() []string { return []string{"moved.example", "correct.example", "missing.example"} }, nil)

	w.checkAll()
	if w.Mismatch("moved.example") != nil {
		t.Fatal("flagged after a single mismatch")
	}
	w.checkAll()
	m := w.Mismatch("moved.example")
	if m == nil || m.ShouldBe != "203.0.113.7" || len(m.ResolvesTo) != 1 || m.ResolvesTo[0] != "198.51.100.1" {
		t.Fatalf("mismatch = %+v", m)
	}
	if w.Mismatch("correct.example") != nil || w.Mismatch("missing.example") != nil {
		t.Error("a matching or unresolvable domain was flagged")
	}

	// The owner fixes the record: one matching check clears the flag.
	dns["moved.example"] = []net.IP{net.ParseIP("203.0.113.7")}
	if w.Recheck(context.Background(), "moved.example") != nil {
		t.Error("flag not cleared after the record was fixed")
	}
}

func TestWatcherIgnoresUntrustedSources(t *testing.T) {
	dns := map[string][]net.IP{"example.com": {net.ParseIP("198.51.100.1")}}
	for name, router := range map[string]*portmap.Router{
		"no router":      nil,
		"shared address": {Protocol: "upnp", ExternalIP: "100.78.241.44"},
		"double NAT":     {Protocol: "upnp", ExternalIP: "192.168.1.20"},
	} {
		d, _ := newTestDetector(router, dns, []string{"192.168.1.20"})
		w := NewWatcher(d, func() []string { return []string{"example.com"} }, nil)
		w.checkAll()
		w.checkAll()
		if w.Mismatch("example.com") != nil {
			t.Errorf("%s: flagged without a trustworthy public IP", name)
		}
	}
}

func TestWatcherStopWithoutStart(t *testing.T) {
	d, _ := newTestDetector(nil, nil, nil)
	w := NewWatcher(d, func() []string { return nil }, nil)
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked on a watcher that was never started")
	}
}
