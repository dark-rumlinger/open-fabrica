package middleware

import (
	"net/http"
	"net/url"
	"strings"
)

// RejectCrossSite refuses state-changing requests a browser reports as coming
// from another origin. The admin session is a SameSite=Strict cookie, which
// stops a different registrable domain but not a sibling subdomain or a project
// served from another port on the same host: a page there could POST text/plain
// JSON with the cookie attached. Browsers always send Sec-Fetch-Site and, on
// POST, Origin, so those are checked. A request with neither (curl, scripts,
// older tooling) is not a browser cross-site request and passes; it must still
// carry credentials.
func RejectCrossSite(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
			u, err := url.Parse(origin)
			// A proxy that rewrites Host reports the public name in X-Forwarded-Host.
			if err != nil || !(strings.EqualFold(u.Host, r.Host) || strings.EqualFold(u.Host, r.Header.Get("X-Forwarded-Host"))) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		} else if origin == "null" {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
