package middleware

import "net/http"

// ServerName is the value sent in the `Server:` response header. Mirrors
// the "Server: nginx" / "Server: Microsoft-IIS/10.0" convention so clients
// (and curious visitors) can identify the underlying engine. We omit the
// version intentionally - exposing it would help attackers target known
// CVEs without any real upside for legitimate clients.
const ServerName = "Open Fabrica"

// SecurityHeaders adds standard security headers to all responses.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", ServerName)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(self), microphone=(self), geolocation=()")
		// HSTS: emit on direct TLS or when the trusted tlsproxy reverse proxy
		// terminated TLS upstream. Without the XFP check, proxied requests
		// (the normal production path) never get HSTS, since r.TLS is nil
		// on the plain HTTP hop from proxy to internal server.
		// No includeSubDomains: the host here is a customer's own domain, and pinning
		// every subdomain of it to HTTPS for two years can break their unrelated
		// plain-HTTP subdomains with no way for them to undo it.
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000")
		}
		next.ServeHTTP(w, r)
	})
}
