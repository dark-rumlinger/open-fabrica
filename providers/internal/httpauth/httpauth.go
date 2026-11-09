// Package httpauth holds request-auth helpers shared across provider transports
// (the OpenAI-compatible LLM clients, the local Ollama/llama.cpp clients, the
// image-generation clients, and the stock/web search adapters). It is the single
// home for turning a providercfg.Auth descriptor into an actual credential on
// the wire, so a vendor with an unusual header does not need its own Go package.
package httpauth

import (
	"net/http"

	"openfabrica/internal/providercfg"
)

// SetBearer sets an "Authorization: Bearer <key>" header on req when key is
// non-empty. A blank key, the normal case for unauthenticated local providers
// such as Ollama or llama.cpp, leaves the header unset, so callers no longer
// need to wrap the header write in their own `if key != ""` guard.
func SetBearer(req *http.Request, key string) {
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// Apply attaches key to req according to a. A blank key is a no-op for every
// scheme, preserving the keyless-local-provider behaviour SetBearer has always
// had: a local runtime that needs no credential must not get an empty header.
//
// providercfg.SchemeBody is deliberately a no-op here. A JSON body is built
// from a typed struct by the transport that owns it, so injecting a field from
// this side would mean re-marshalling someone else's payload. Transports on
// that scheme read BodyKey and set the field themselves.
func Apply(req *http.Request, a providercfg.Auth, key string) {
	if key == "" {
		return
	}
	switch a.Effective() {
	case providercfg.SchemeHeader:
		req.Header.Set(a.Header, a.Prefix+key)
	case providercfg.SchemeQuery:
		q := req.URL.Query()
		q.Set(a.Param, key)
		req.URL.RawQuery = q.Encode()
	case providercfg.SchemeNone, providercfg.SchemeBody:
		// none: the provider wants no credential.
		// body: the transport injects it into the payload (see BodyKey).
	default: // providercfg.SchemeBearer
		SetBearer(req, key)
	}
}

// BodyKey returns the JSON field name a body-scheme credential goes under, or
// "" when a uses any other scheme. Lets a transport ask "does my payload need
// to carry the key?" without switching on the scheme constant itself.
func BodyKey(a providercfg.Auth) string {
	if a.Effective() == providercfg.SchemeBody {
		return a.Param
	}
	return ""
}

// SetExtra applies static per-vendor headers (Anthropic's anthropic-version,
// OpenRouter's attribution pair, Unsplash's Accept-Version). Called on every
// request alongside Apply.
func SetExtra(req *http.Request, extra map[string]string) {
	for k, v := range extra {
		req.Header.Set(k, v)
	}
}
