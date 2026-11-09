package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A policy carrying the {nonce} token must get a fresh nonce substituted into the
// header AND exposed on the request context (so the renderer can stamp inline
// scripts). This is the mechanism the hardened CSP preset relies on.
func TestHTMLContentSecurityPolicy_NonceSubstitution(t *testing.T) {
	var ctxNonce string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxNonce = NonceFromContext(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	})
	mw := HTMLContentSecurityPolicy(func(*http.Request) string {
		return "default-src 'self'; script-src 'self' {nonce}"
	})
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if ctxNonce == "" {
		t.Fatal("expected a nonce on the request context")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "{nonce}") {
		t.Errorf("placeholder not substituted: %q", csp)
	}
	if !strings.Contains(csp, "'nonce-"+ctxNonce+"'") {
		t.Errorf("header missing the minted nonce %q: %q", ctxNonce, csp)
	}
}

// A policy that carries no {nonce} token is passed through verbatim and mints no
// nonce - minting one anyway would cost entropy per request and hand the renderer
// a nonce no policy honours, so inline scripts stamped with it would be blocked.
func TestHTMLContentSecurityPolicy_NoNonceWhenNoToken(t *testing.T) {
	var ctxNonce string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxNonce = NonceFromContext(r)
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
	})
	mw := HTMLContentSecurityPolicy(func(*http.Request) string { return "default-src 'self'" })
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if ctxNonce != "" {
		t.Errorf("expected no nonce on the context, got %q", ctxNonce)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("policy must pass through verbatim, got %q", got)
	}
}

// The header is HTML-only: a non-text/html response must not carry a CSP header
// even when a policy is configured.
func TestHTMLContentSecurityPolicy_NonHTMLGetsNoHeader(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	})
	mw := HTMLContentSecurityPolicy(func(*http.Request) string { return "default-src 'self'" })
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("non-HTML response must have no CSP header, got %q", got)
	}
}

// An empty resolved policy disables the header entirely (the opt-out path).
func TestHTMLContentSecurityPolicy_EmptyPolicySkips(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
	})
	mw := HTMLContentSecurityPolicy(func(*http.Request) string { return "" })
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("empty policy must set no header, got %q", got)
	}
}
