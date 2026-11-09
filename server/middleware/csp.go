package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// nonceToken is the literal placeholder a CSP policy uses to opt into per-request
// nonces. When present, it is replaced with a fresh 'nonce-<base64>' source and
// the bare nonce is exposed on the request context (NonceFromContext) so the page
// renderer can stamp it onto every inline <script> it emits. This lets a project
// adopt a strict, nonce-based script-src (dropping 'unsafe-inline') while inline
// scripts keep working.
const nonceToken = "{nonce}"

type ctxKey int

const nonceCtxKey ctxKey = 0

// NonceFromContext returns the per-request CSP nonce set by
// HTMLContentSecurityPolicy when the resolved policy used the {nonce} token, or
// "" when this response has no nonce (the common, permissive-default case).
func NonceFromContext(r *http.Request) string {
	if v, ok := r.Context().Value(nonceCtxKey).(string); ok {
		return v
	}
	return ""
}

// newCSPNonce returns a fresh base64 nonce, or "" if the system RNG fails.
func newCSPNonce() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// HTMLContentSecurityPolicy returns middleware that sets the
// Content-Security-Policy header on HTML responses only. The Content-Type check
// runs at WriteHeader time so it sees the type the handler actually set, not the
// request's Accept. The policy is resolved per request (per-project override
// falling back to the instance default); "" skips the header. A {nonce} token in
// the policy mints a fresh nonce, substitutes it in, and exposes it on the
// request context so downstream handlers can stamp it onto inline scripts.
func HTMLContentSecurityPolicy(resolve func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			policy := resolve(r)
			if policy == "" {
				next.ServeHTTP(w, r)
				return
			}
			if strings.Contains(policy, nonceToken) {
				if nonce := newCSPNonce(); nonce != "" {
					policy = strings.ReplaceAll(policy, nonceToken, "'nonce-"+nonce+"'")
					r = r.WithContext(context.WithValue(r.Context(), nonceCtxKey, nonce))
				} else {
					// RNG failure: drop the placeholder rather than emit a literal
					// {nonce} the browser would reject as an invalid source.
					policy = strings.ReplaceAll(policy, nonceToken, "")
				}
			}
			next.ServeHTTP(&cspWriter{ResponseWriter: w, policy: policy}, r)
		})
	}
}

type cspWriter struct {
	http.ResponseWriter
	policy      string
	wroteHeader bool
}

func (cw *cspWriter) WriteHeader(code int) {
	cw.applyCSP()
	cw.wroteHeader = true
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *cspWriter) Write(b []byte) (int, error) {
	if !cw.wroteHeader {
		cw.applyCSP()
		cw.wroteHeader = true
	}
	return cw.ResponseWriter.Write(b)
}

func (cw *cspWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *cspWriter) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

// Hijack forwards to the underlying writer so WebSocket/upgrade handlers can take
// over the connection. Without this, websocket.Accept's hijack fails when CSP is
// enabled (chi's compressor asserts http.Hijacker on this wrapper, not following
// Unwrap), stranding the socket half-upgraded with no frames ever sent.
func (cw *cspWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := cw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("csp middleware: underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

func (cw *cspWriter) applyCSP() {
	if strings.HasPrefix(strings.ToLower(cw.Header().Get("Content-Type")), "text/html") {
		cw.Header().Set("Content-Security-Policy", cw.policy)
	}
}
