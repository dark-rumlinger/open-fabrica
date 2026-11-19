package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRejectCrossSite(t *testing.T) {
	h := RejectCrossSite(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"same-origin POST", "POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://localhost:5734"}, 200},
		{"POST without browser headers", "POST", nil, 200},
		{"sibling site POST", "POST", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://app.localhost:5733"}, 403},
		{"cross-site POST", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"foreign Origin without fetch metadata", "DELETE", map[string]string{"Origin": "http://evil.example"}, 403},
		{"opaque origin", "PUT", map[string]string{"Origin": "null"}, 403},
		{"proxy that rewrites Host", "POST", map[string]string{"Origin": "http://localhost:5734", "X-Forwarded-Host": "localhost:5734"}, 200},
		{"cross-site GET is not state-changing", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://localhost:5734/admin/api/projects", nil)
		req.Host = "localhost:5734"
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}

	// Untrusted peer cannot spoof X-Forwarded-Host to bypass Origin check.
	untrusted := httptest.NewRequest("POST", "http://localhost:5734/admin/api/projects", nil)
	untrusted.Host = "localhost:5734"
	untrusted.RemoteAddr = "203.0.113.10:12345"
	untrusted.Header.Set("Origin", "https://evil.example")
	untrusted.Header.Set("X-Forwarded-Host", "evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, untrusted)
	if rec.Code != http.StatusForbidden {
		t.Errorf("untrusted peer X-Forwarded-Host spoof: status %d, want 403", rec.Code)
	}

	// Loopback (trusted) proxy rewriting Host is accepted.
	trusted := httptest.NewRequest("POST", "http://127.0.0.1:5734/admin/api/projects", nil)
	trusted.Host = "127.0.0.1:5734"
	trusted.RemoteAddr = "127.0.0.1:54321"
	trusted.Header.Set("Origin", "https://app.example")
	trusted.Header.Set("X-Forwarded-Host", "app.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, trusted)
	if rec.Code != http.StatusOK {
		t.Errorf("trusted proxy X-Forwarded-Host: status %d, want 200", rec.Code)
	}
}
