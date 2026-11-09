// Package security provides the platform's shared crypto and request-hardening
// primitives: value encryption, password hashing, HMAC signing, JWT issuance
// and validation middleware, and SQL identifier validation.
package security

import (
	"context"
	"net/http"
	"strings"

	"openfabrica/internal/httpx"
)

type contextKey string

const ClaimsKey contextKey = "claims"

// Authenticator is chi middleware that validates the JWT from the Authorization
// header or the of_auth cookie. It deliberately does NOT accept a token from the
// URL query string: query-string tokens leak into access logs, browser history,
// and Referer headers. SSE/EventSource routes that genuinely cannot set a header
// use AuthenticatorAllowingQueryToken instead, so the query-param path is confined
// to exactly those routes.
func (j *JWTManager) Authenticator(next http.Handler) http.Handler {
	return j.authenticate(next, false)
}

// AuthenticatorAllowingQueryToken is Authenticator plus acceptance of the JWT
// from a ?token= query parameter, for EventSource/SSE endpoints that cannot set
// an Authorization header and are reached by a client without the auth cookie.
// Mount it ONLY on such SSE routes, never on the whole admin surface, because
// the query-string token leaks into logs/history/referrers.
func (j *JWTManager) AuthenticatorAllowingQueryToken(next http.Handler) http.Handler {
	return j.authenticate(next, true)
}

func (j *JWTManager) authenticate(next http.Handler, allowQueryToken bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var tokenString string

		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			tokenString = strings.TrimPrefix(authHeader, "Bearer ")
			if tokenString == authHeader {
				httpx.WriteError(w, http.StatusUnauthorized, "invalid authorization format")
				return
			}
		} else if cookie, err := r.Cookie(AuthCookieName); err == nil && cookie.Value != "" {
			tokenString = cookie.Value
		} else if qToken := r.URL.Query().Get("token"); allowQueryToken && qToken != "" {
			// SSE/EventSource only: these clients cannot set custom headers.
			// Enabled solely on SSE routes (see AuthenticatorAllowingQueryToken).
			tokenString = qToken
		} else {
			httpx.WriteError(w, http.StatusUnauthorized, "missing authorization")
			return
		}

		claims, err := j.Validate(tokenString)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}

		ctx := context.WithValue(r.Context(), ClaimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole middleware checks that the authenticated user has the required role
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := r.Context().Value(ClaimsKey).(*Claims)
			if !ok || claims.Role != role {
				httpx.WriteError(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
