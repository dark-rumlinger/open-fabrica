package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":         true,
		"localhost:5734":    true,
		"LOCALHOST.":        true,
		"127.0.0.1:5734":    true,
		"[::1]:5734":        true,
		"::1":               true,
		"app.localhost":     true,
		"evil.example.com":  false,
		"127.0.0.1.evil.io": false,
		"localhost.evil.io": false,
		"10.0.0.5:5734":     false,
		"":                  false,
	} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLoopbackHostOnly(t *testing.T) {
	h := LoopbackHostOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	for host, want := range map[string]int{
		"localhost:5734":   http.StatusOK,
		"127.0.0.1:5734":   http.StatusOK,
		"rebind.evil.test": http.StatusMisdirectedRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/triage", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, rec.Code, want)
		}
	}
}

func TestHasProxyHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if HasProxyHeaders(req) {
		t.Error("a direct request has no proxy headers")
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	if !HasProxyHeaders(req) {
		t.Error("X-Forwarded-For marks a proxied request")
	}
}
