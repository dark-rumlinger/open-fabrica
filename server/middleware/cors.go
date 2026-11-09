package middleware

import (
	"log/slog"
	"net/http"
)

// CORS returns middleware that sets Cross-Origin Resource Sharing headers.
// In development (when allowedOrigins is empty or contains "*"), all origins
// are permitted. Otherwise, only the listed origins are allowed. Every OPTIONS
// preflight is answered here with 204. Use CORSWithPreflightPassthrough for the
// variant that lets specific routes answer their own (narrower) preflight.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	return CORSWithPreflightPassthrough(allowedOrigins, nil)
}

// CORSWithPreflightPassthrough is CORS with one extra hook: when passthrough
// matches a request, an OPTIONS preflight is NOT short-circuited here - the
// coarse instance-wide headers are still written, but the downstream handler
// answers the preflight with its own per-route policy. Without this, a
// router-answered 204 reflecting the permissive instance default "*" would
// silently defeat a restrictive per-endpoint allowlist on OPTIONS. For every
// non-matching request, behavior is identical to CORS.
func CORSWithPreflightPassthrough(allowedOrigins []string, passthrough func(*http.Request) bool) func(http.Handler) http.Handler {
	allowAll := len(allowedOrigins) == 0
	if !allowAll {
		for _, o := range allowedOrigins {
			if o == "*" {
				allowAll = true
				break
			}
		}
	}

	if allowAll {
		slog.Warn("CORS: allowing all origins (Access-Control-Allow-Origin: *)")
	}

	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				// Credentials are incompatible with wildcard origin per spec.
			} else if origin != "" {
				if _, ok := originSet[origin]; ok {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					w.Header().Set("Vary", "Origin")
				}
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")

			if r.Method == http.MethodOptions {
				// A route that answers its own preflight downstream (per-endpoint
				// CORS) opts out of the short-circuit so it - not this coarse
				// instance-wide layer - decides whether the preflight is allowed.
				if passthrough != nil && passthrough(r) {
					next.ServeHTTP(w, r)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
