package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The unauthenticated, loopback-only routes must refuse a request a same-host
// reverse proxy relayed: its peer is 127.0.0.1, but the caller is not the operator.
func TestRequireLoopbackRejectsProxiedRequests(t *testing.T) {
	h := requireLoopback(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	call := func(remote string, header map[string]string) int {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/triage", nil)
		req.RemoteAddr = remote
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := call("127.0.0.1:50000", nil); got != http.StatusOK {
		t.Errorf("direct loopback: %d, want 200", got)
	}
	if got := call("127.0.0.1:50000", map[string]string{"X-Forwarded-For": "203.0.113.9"}); got != http.StatusForbidden {
		t.Errorf("proxied loopback: %d, want 403", got)
	}
	if got := call("198.51.100.4:50000", nil); got != http.StatusForbidden {
		t.Errorf("remote peer: %d, want 403", got)
	}
}
