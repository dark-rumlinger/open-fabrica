// Package providerreg is a generic, transport-keyed constructor registry shared
// by the llm, imagegen, stocksearch, and websearch packages, parameterized over
// each domain's Provider interface. Thin domain wrappers keep the original
// public APIs.
package providerreg

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"

	"openfabrica/internal/providercfg"
)

// Options is everything a backend needs to construct itself. It replaced a
// positional (name, apiKey, baseURL, hc) signature so that Config could reach
// the transports at all: without it, per-vendor behaviour had nowhere to live
// but a Go wrapper type, and "add a provider" meant "add a package".
//
// BaseURL is a ROOT, never a full operation endpoint - "https://api.openai.com/v1",
// not ".../v1/chat/completions". Backends derive their paths with Endpoint and
// Sibling rather than by string surgery on a suffix that may not be there.
type Options struct {
	// Name is the providers.name row value. It is the runtime instance key
	// and the capability-store key, so it must stay stable for a given row.
	Name string
	// APIKey is the decrypted credential, or "" for keyless local providers.
	APIKey string
	// BaseURL is the API root. Empty means the backend's own default.
	BaseURL string
	// ModelsURL is the catalog endpoint. Empty means the backend derives one
	// from BaseURL; when set it is authoritative, which is how a provider
	// that lists on an unrelated path stays configuration rather than code.
	ModelsURL string
	// HTTPClient is optional; nil means the backend installs its own
	// default-timeout client.
	HTTPClient *http.Client
	// Config is the decoded providers.config blob.
	Config providercfg.Config
}

// Endpoint joins segments onto the base URL root:
//
//	"https://api.openai.com/v1" + ("chat", "completions")
//	  -> "https://api.openai.com/v1/chat/completions"
//
// fallbackBase is used when Options.BaseURL is empty. Returns the joined string
// unparsed if the base is not a valid URL, so a misconfigured row surfaces as a
// request error naming the bad URL rather than as a silent empty endpoint.
func (o Options) Endpoint(fallbackBase string, parts ...string) string {
	return join(o.base(fallbackBase), false, parts...)
}

// Sibling replaces the FINAL path segment of the base URL root with segments:
//
//	"http://localhost:11434/v1"        + ("api", "tags") -> "http://localhost:11434/api/tags"
//	"https://proxy.example/ollama/v1"  + ("api", "tags") -> "https://proxy.example/ollama/api/tags"
//
// Ollama's native /api/chat and llama.cpp's /props sit beside the OpenAI-shaped
// /v1 surface rather than under it. Replacing the last segment (instead of
// discarding the whole path) keeps a provider mounted behind a path prefix
// working, which stripping to the origin would break.
func (o Options) Sibling(fallbackBase string, parts ...string) string {
	return join(o.base(fallbackBase), true, parts...)
}

func (o Options) base(fallbackBase string) string {
	if strings.TrimSpace(o.BaseURL) != "" {
		return o.BaseURL
	}
	return fallbackBase
}

func join(base string, dropLast bool, parts ...string) string {
	base = strings.TrimRight(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		// Not parseable as an absolute URL. Fall back to plain concatenation
		// so the caller still gets a string that names the misconfiguration.
		return strings.TrimRight(base+"/"+path.Join(parts...), "/")
	}
	p := u.Path
	if dropLast {
		p = path.Dir(p)
	}
	u.Path = path.Join(append([]string{p}, parts...)...)
	return u.String()
}

// Constructor is the shared shape every backend uses.
type Constructor[P any] func(Options) P

// Registry holds the transport -> constructor map for one provider domain.
// label is used in error messages so duplicate registrations and unknown
// transports surface their domain ("llm", "imagegen", "stocksearch",
// "websearch") instead of a bare key.
type Registry[P any] struct {
	label string
	mu    sync.RWMutex
	ctors map[string]Constructor[P]
	errs  []error
}

func New[P any](label string) *Registry[P] {
	return &Registry[P]{label: label, ctors: map[string]Constructor[P]{}}
}

// Register installs a constructor for a transport identifier. Duplicate
// registration returns an error and accumulates it for RegistrationErrors;
// main() drains those and exits, so an init()-time duplicate does not crash
// with a stack trace that hides sibling registration problems.
func (r *Registry[P]) Register(transport string, c Constructor[P]) error {
	key := strings.ToLower(transport)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.ctors[key]; exists {
		err := fmt.Errorf("%s: duplicate provider registration: %s", r.label, key)
		r.errs = append(r.errs, err)
		return err
	}
	r.ctors[key] = c
	return nil
}

// RegistrationErrors returns errors recorded during init()-time Register
// calls. Returns nil if none.
func (r *Registry[P]) RegistrationErrors() []error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.errs) == 0 {
		return nil
	}
	out := make([]error, len(r.errs))
	copy(out, r.errs)
	return out
}

// New constructs a Provider for the given transport. Returns the zero value of
// P and a descriptive error if no backend is registered.
func (r *Registry[P]) New(transport string, opts Options) (P, error) {
	r.mu.RLock()
	c, ok := r.ctors[strings.ToLower(transport)]
	r.mu.RUnlock()
	var zero P
	if !ok {
		return zero, fmt.Errorf("unknown %s transport: %q", r.label, transport)
	}
	return c(opts), nil
}

// Has reports whether a constructor is registered for the transport.
func (r *Registry[P]) Has(transport string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.ctors[strings.ToLower(transport)]
	return ok
}

// Types returns the sorted list of registered transport identifiers.
// Sorted so admin UIs and tests get deterministic output.
func (r *Registry[P]) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.ctors))
	for k := range r.ctors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
