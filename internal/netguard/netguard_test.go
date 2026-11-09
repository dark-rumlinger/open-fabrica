package netguard

import (
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestValidateExternalURLAllowingHost pins the scoped private-host opt-in: the
// default path still blocks private/internal IPs, an explicitly allowed host may
// resolve private, a *different* private host stays blocked, and the http/https
// scheme check applies even to the allowed host. IP literals are used so the
// validator never touches the network (LookupHost returns an IP literal as-is).
func TestValidateExternalURLAllowingHost(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		allowHost string
		wantErr   bool
	}{
		{name: "public ip, no opt-in, allowed", url: "http://93.184.216.34/x", allowHost: "", wantErr: false},
		{name: "private ip, no opt-in, blocked", url: "http://192.168.1.50/x", allowHost: "", wantErr: true},
		{name: "loopback, no opt-in, blocked", url: "http://127.0.0.1:8123/x", allowHost: "", wantErr: true},
		{name: "private ip, matching opt-in, allowed", url: "http://192.168.1.50/x", allowHost: "192.168.1.50", wantErr: false},
		{name: "loopback, matching opt-in, allowed", url: "http://127.0.0.1:8123/x", allowHost: "127.0.0.1", wantErr: false},
		{name: "private ip, different opt-in host, blocked", url: "http://192.168.1.50/x", allowHost: "192.168.1.99", wantErr: true},
		{name: "non-http scheme rejected even when allowed", url: "ftp://192.168.1.50/x", allowHost: "192.168.1.50", wantErr: true},
		{name: "file scheme rejected even when allowed", url: "file:///etc/passwd", allowHost: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExternalURLAllowingHost(tc.url, tc.allowHost)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q (allowHost=%q), got nil", tc.url, tc.allowHost)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error for %q (allowHost=%q), got %v", tc.url, tc.allowHost, err)
			}
		})
	}
}

// TestValidateExternalURLDelegates confirms the unparameterized ValidateExternalURL
// still behaves as before (it now delegates to the host-aware variant with no
// allowed host): public passes, private blocks.
func TestValidateExternalURLDelegates(t *testing.T) {
	if err := ValidateExternalURL("http://93.184.216.34/x"); err != nil {
		t.Fatalf("public URL should pass, got %v", err)
	}
	if err := ValidateExternalURL("http://10.0.0.1/x"); err == nil {
		t.Fatalf("private URL should be blocked")
	}
}

// TestIsPrivateIP pins the address ranges an external fetch must never reach,
// including the two that plain net.IP helpers miss: RFC-6598 CGNAT space and
// IPv4-mapped IPv6 encodings of otherwise-private addresses.
func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		// Public, must be allowed.
		{"93.184.216.34", false},
		{"8.8.8.8", false},
		{"2606:2800:220:1:248:1893:25c8:1946", false},
		// Loopback / unspecified / link-local.
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"169.254.169.254", true}, // cloud metadata (link-local)
		{"fe80::1", true},
		// RFC-1918 private.
		{"10.0.0.1", true},
		{"192.168.1.50", true},
		{"172.16.0.1", true},
		{"fc00::1", true}, // IPv6 unique-local
		// RFC-6598 CGNAT (the gap this test guards).
		{"100.64.0.1", true},
		{"100.127.255.255", true},
		{"100.63.255.255", false}, // just below the /10 boundary, still public
		{"100.128.0.0", false},    // just above the /10 boundary, still public
		// IPv4-mapped IPv6 must be unmapped before the range check.
		{"::ffff:10.0.0.1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:100.64.0.1", true},
		{"::ffff:93.184.216.34", false},
		// Multicast, "this network", reserved and benchmarking space.
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"ff02::1", true},
		{"ff0e::1", true},
		{"0.1.2.3", true},
		{"198.18.0.1", true},
		{"255.255.255.255", true},
		// IPv6 transition addresses wrapping an IPv4 host.
		{"64:ff9b::7f00:1", true},                      // NAT64 -> 127.0.0.1
		{"64:ff9b::a00:1", true},                       // NAT64 -> 10.0.0.1
		{"64:ff9b::808:808", false},                    // NAT64 -> 8.8.8.8
		{"2002:7f00:1::", true},                        // 6to4 -> 127.0.0.1
		{"2002:c0a8:101::", true},                      // 6to4 -> 192.168.1.1
		{"2002:808:808::", false},                      // 6to4 -> 8.8.8.8
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", true}, // Teredo
		{"::127.0.0.1", true},                          // IPv4-compatible
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("unparseable test IP %q", c.ip)
		}
		if got := IsPrivateIP(ip); got != c.private {
			t.Errorf("IsPrivateIP(%q) = %v, want %v", c.ip, got, c.private)
		}
	}
}

// TestSSRFSafeClientAllowingHostRedirectGuard exercises the client wiring without
// dialing: the CheckRedirect hook must block a redirect to a private host unless
// that host is the scoped allowed host.
func TestSSRFSafeClientAllowingHostRedirectGuard(t *testing.T) {
	priv, _ := http.NewRequest("GET", "http://192.168.1.50/next", nil)

	// No opt-in: a redirect to a private host is rejected.
	if err := SSRFSafeClient().CheckRedirect(priv, nil); err == nil {
		t.Fatalf("expected redirect to private host to be blocked")
	}

	// Matching opt-in: the same host is permitted on redirect.
	if err := SSRFSafeClientAllowingHost("192.168.1.50").CheckRedirect(priv, nil); err != nil {
		t.Fatalf("expected allowed-host redirect to pass, got %v", err)
	}

	// Different opt-in host: still blocked.
	if err := SSRFSafeClientAllowingHost("192.168.1.99").CheckRedirect(priv, nil); err == nil {
		t.Fatalf("expected redirect to a non-allowed private host to be blocked")
	}

	// Redirect loop guard still fires.
	via := make([]*http.Request, 10)
	if err := SSRFSafeClient().CheckRedirect(priv, via); err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("expected too-many-redirects guard, got %v", err)
	}
}
