package middleware

import (
	"net/http/httptest"
	"testing"
)

func clientIPFor(t *testing.T, remote string, headers map[string][]string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = remote
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return ClientIP(req)
}

func TestClientIPReadsTheChainFromTheRight(t *testing.T) {
	SetTrustedProxies(nil)
	// nginx on the same host appends the peer it saw; the leftmost entry is
	// whatever the caller sent and must not be believed.
	got := clientIPFor(t, "127.0.0.1:4000", map[string][]string{"X-Forwarded-For": {"6.6.6.6, 203.0.113.9"}})
	if got != "203.0.113.9" {
		t.Errorf("spoofed leftmost entry won: %q", got)
	}
	// Split across header lines is the same chain.
	got = clientIPFor(t, "127.0.0.1:4000", map[string][]string{"X-Forwarded-For": {"6.6.6.6", "203.0.113.9"}})
	if got != "203.0.113.9" {
		t.Errorf("multi-line header: %q", got)
	}
	// The built-in proxy sends a single true address.
	if got := clientIPFor(t, "127.0.0.1:4000", map[string][]string{"X-Forwarded-For": {"198.51.100.7"}}); got != "198.51.100.7" {
		t.Errorf("single entry: %q", got)
	}
}

func TestClientIPIgnoresHeadersFromUntrustedPeers(t *testing.T) {
	SetTrustedProxies(nil)
	got := clientIPFor(t, "198.51.100.20:5000", map[string][]string{"X-Forwarded-For": {"1.2.3.4"}, "X-Real-Ip": {"1.2.3.4"}})
	if got != "198.51.100.20" {
		t.Errorf("headers from a direct peer were believed: %q", got)
	}
}

func TestClientIPTrustedProxyNetworks(t *testing.T) {
	if errs := SetTrustedProxies([]string{"10.0.0.0/8", "172.18.0.5", "not-an-ip"}); len(errs) != 1 {
		t.Fatalf("want exactly the bad entry reported, got %v", errs)
	}
	t.Cleanup(func() { SetTrustedProxies(nil) })

	// A proxy on a container network, one hop in front of the server.
	if got := clientIPFor(t, "10.1.2.3:80", map[string][]string{"X-Forwarded-For": {"203.0.113.50"}}); got != "203.0.113.50" {
		t.Errorf("configured proxy network not trusted: %q", got)
	}
	// Two proxies: the inner one is skipped, the visitor is the first untrusted hop.
	if got := clientIPFor(t, "10.1.2.3:80", map[string][]string{"X-Forwarded-For": {"203.0.113.50, 10.9.9.9"}}); got != "203.0.113.50" {
		t.Errorf("trusted inner hop not skipped: %q", got)
	}
	// An address outside the list is still just a peer.
	if got := clientIPFor(t, "192.0.2.77:80", map[string][]string{"X-Forwarded-For": {"203.0.113.50"}}); got != "192.0.2.77" {
		t.Errorf("unlisted peer was trusted: %q", got)
	}
}

func TestClientIPFallsBackToXRealIPWithoutAChain(t *testing.T) {
	SetTrustedProxies(nil)
	if got := clientIPFor(t, "127.0.0.1:4000", map[string][]string{"X-Real-Ip": {"203.0.113.77"}}); got != "203.0.113.77" {
		t.Errorf("X-Real-IP from a loopback proxy: %q", got)
	}
	if got := clientIPFor(t, "127.0.0.1:4000", nil); got != "127.0.0.1" {
		t.Errorf("no headers: %q", got)
	}
}
