package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORS pins the router-level CORS semantics that config.json cors_origins
// relies on once it is wired to the public router: an empty (or wildcard)
// allowlist is permissive "*" with no credentials, a real allowlist reflects a
// matched Origin with credentials + Vary and stays silent for everything else,
// and an OPTIONS preflight short-circuits with 204 without reaching the handler.
func TestCORS(t *testing.T) {
	cases := []struct {
		name       string
		allowed    []string
		method     string
		origin     string
		wantAllow  string // Access-Control-Allow-Origin ("" = header absent)
		wantCreds  string // Access-Control-Allow-Credentials ("" = header absent)
		wantVary   string // Vary ("" = header absent)
		wantStatus int
		wantNext   bool // did the wrapped handler run?
	}{
		// Empty allowlist == permissive default: this is what an unset cors_origins
		// produces on the public router.
		{"empty list allows all, no credentials", nil, http.MethodGet, "https://a.com", "*", "", "", http.StatusOK, true},
		{"explicit wildcard allows all", []string{"*"}, http.MethodGet, "https://a.com", "*", "", "", http.StatusOK, true},

		// Real allowlist: reflect a matched origin with credentials, block the rest.
		{"allowlist reflects matched origin", []string{"https://a.com", "https://b.com"}, http.MethodGet, "https://b.com", "https://b.com", "true", "Origin", http.StatusOK, true},
		{"allowlist blocks unmatched origin", []string{"https://a.com"}, http.MethodGet, "https://evil.com", "", "", "", http.StatusOK, true},
		{"allowlist with no Origin header stays silent", []string{"https://a.com"}, http.MethodGet, "", "", "", "", http.StatusOK, true},

		// OPTIONS preflight short-circuits (204) and never reaches the handler.
		{"preflight matched origin short-circuits", []string{"https://a.com"}, http.MethodOptions, "https://a.com", "https://a.com", "true", "Origin", http.StatusNoContent, false},
		{"preflight unmatched origin short-circuits without allow", []string{"https://a.com"}, http.MethodOptions, "https://evil.com", "", "", "", http.StatusNoContent, false},
		{"preflight allow-all short-circuits", nil, http.MethodOptions, "https://a.com", "*", "", "", http.StatusNoContent, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextCalled := false
			h := CORS(tc.allowed)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(tc.method, "/api/thing", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.wantAllow)
			}
			if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != tc.wantCreds {
				t.Errorf("Allow-Credentials = %q, want %q", got, tc.wantCreds)
			}
			if got := rec.Header().Get("Vary"); got != tc.wantVary {
				t.Errorf("Vary = %q, want %q", got, tc.wantVary)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if nextCalled != tc.wantNext {
				t.Errorf("next handler called = %v, want %v", nextCalled, tc.wantNext)
			}
		})
	}
}

// TestCORSWithPreflightPassthrough pins the public-router behavior: a preflight on
// a passthrough-matched route (the dynamic /api/*) is NOT short-circuited here -
// the coarse instance-wide headers are still written but the request flows to the
// downstream handler, which answers the preflight with its own per-endpoint
// policy. A preflight on any other route still short-circuits with 204, and every
// non-OPTIONS request always reaches the handler.
func TestCORSWithPreflightPassthrough(t *testing.T) {
	passthrough := func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/") }

	cases := []struct {
		name       string
		method     string
		path       string
		wantNext   bool
		wantStatus int
	}{
		{"preflight on /api/* flows downstream", http.MethodOptions, "/api/contacts", true, http.StatusOK},
		{"preflight on non-api short-circuits", http.MethodOptions, "/files/app.js", false, http.StatusNoContent},
		{"GET on /api/* flows downstream", http.MethodGet, "/api/contacts", true, http.StatusOK},
		{"GET on non-api flows downstream", http.MethodGet, "/files/app.js", true, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextCalled := false
			h := CORSWithPreflightPassthrough([]string{"*"}, passthrough)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Origin", "https://a.com")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if nextCalled != tc.wantNext {
				t.Errorf("next handler called = %v, want %v", nextCalled, tc.wantNext)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			// The coarse headers are written regardless of passthrough, so the
			// downstream handler (and the short-circuit) both start from the
			// instance-wide policy.
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Allow-Origin = %q, want * (coarse headers always written)", got)
			}
		})
	}
}
