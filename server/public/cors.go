package public

import (
	"net/http"
	"strings"
)

// setEndpointCORSOrigin writes the Access-Control-Allow-Origin (and, on a match,
// Allow-Credentials + Vary) header for a per-endpoint CORS spec. "*" allows any
// origin; otherwise the request's Origin must exactly match a comma-separated
// corsOrigins entry. Writes nothing when corsOrigins is empty; callers own
// Allow-Methods/Allow-Headers. Any inherited Allow-Origin/Allow-Credentials is
// dropped FIRST, or the router's permissive "*" default would linger and defeat
// the per-endpoint restriction. Single source of truth for endpoint origin
// matching, shared by the dynamic API, function endpoints, and LLM endpoints.
func setEndpointCORSOrigin(w http.ResponseWriter, corsOrigins, origin string) {
	if corsOrigins == "" {
		return
	}
	if corsOrigins == "*" {
		// "*" means any origin, no credentials (the pair is invalid per spec and
		// browsers reject it). Drop any credentials the coarse layer set for a
		// matched instance-wide origin so this wildcard doesn't carry a dangling
		// Allow-Credentials.
		w.Header().Del("Access-Control-Allow-Credentials")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		return
	}
	// This allowlist overrides the coarse layer: clear whatever it set so a
	// non-match leaks nothing, then reflect only an exact match.
	w.Header().Del("Access-Control-Allow-Origin")
	w.Header().Del("Access-Control-Allow-Credentials")
	if origin == "" {
		return
	}
	for _, allowed := range strings.Split(corsOrigins, ",") {
		if strings.TrimSpace(allowed) == origin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
			return
		}
	}
}

// applyEndpointCORS writes the per-endpoint origin plus the Allow-Methods and
// Allow-Headers a preflight needs, so each handler's CORS block is one line.
func applyEndpointCORS(w http.ResponseWriter, corsOrigins, origin, methods, headers string) {
	setEndpointCORSOrigin(w, corsOrigins, origin)
	w.Header().Set("Access-Control-Allow-Methods", methods)
	w.Header().Set("Access-Control-Allow-Headers", headers)
}
