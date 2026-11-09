// Package providerbase holds the authed-HTTP connection base shared by the
// image-generation clients and the stock/web search adapters (the LLM clients
// build their own, see providers/llm/openai and providers/llm/anthropic). Each client
// embeds *BaseClient and supplies only the parts that genuinely differ (request
// body, response parsing, capabilities), so one base serves every domain and
// they cannot drift apart. The per-domain constructor default (e.g. the HTTP
// timeout) stays in that domain's thin NewBaseClient wrapper.
package providerbase

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"openfabrica/internal/providercfg"
	"openfabrica/internal/providerreg"
	"openfabrica/providers/internal/httpauth"
)

// BaseClient holds the connection fields every provider client needs and
// centralizes the constructor defaults shared across the adapter subpackages.
type BaseClient struct {
	name       string
	apiKey     string
	baseURL    string // never has a trailing slash
	httpClient *http.Client
	cfg        providercfg.Config
}

// Defaults is a transport's own vendor knowledge: the credential placement and
// any static headers that vendor requires. Layered UNDER the row's config, so a
// plain seed row needs no auth block and an operator fronting the vendor with a
// proxy can still override it.
type Defaults struct {
	BaseURL string
	Auth    providercfg.Auth
	Headers map[string]string
}

// NewBaseClient applies the shared defaults: fall back to def.BaseURL when
// Options.BaseURL is empty, strip a trailing slash, give the client the
// supplied timeout when Options.HTTPClient is nil, and layer def's auth and
// headers under whatever the row's config states.
func NewBaseClient(o providerreg.Options, def Defaults, timeout time.Duration) *BaseClient {
	defaultBaseURL := def.BaseURL
	baseURL := o.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	return &BaseClient{
		name:       o.Name,
		apiKey:     o.APIKey,
		baseURL:    baseURL,
		httpClient: hc,
		cfg:        o.Config.WithDefaults(def.Auth, def.Headers),
	}
}

// Name satisfies Provider.Name for every adapter.
func (c *BaseClient) Name() string { return c.name }

// APIKey, BaseURL, HTTPClient, and Config expose the connection fields to
// adapters in sibling packages, where the unexported struct fields aren't
// reachable.
func (c *BaseClient) APIKey() string             { return c.apiKey }
func (c *BaseClient) BaseURL() string            { return c.baseURL }
func (c *BaseClient) HTTPClient() *http.Client   { return c.httpClient }
func (c *BaseClient) Config() providercfg.Config { return c.cfg }

// Authorize attaches the credential and any static per-vendor headers to req.
// Every adapter calls exactly this, which is what lets a vendor with an unusual
// auth header (Unsplash's "Client-ID <key>", Brave's X-Subscription-Token,
// Pixabay's ?key=) be a config row rather than a Go package. Adapters on the
// body scheme read BodyKey instead - a typed payload struct cannot be mutated
// from here.
func (c *BaseClient) Authorize(req *http.Request) {
	httpauth.Apply(req, c.cfg.Auth, c.apiKey)
	httpauth.SetExtra(req, c.cfg.ExtraHeaders)
}

// BodyKey returns the JSON field the credential goes under when this provider
// uses the body auth scheme, or "" otherwise. Tavily is the only user.
func (c *BaseClient) BodyKey() string { return httpauth.BodyKey(c.cfg.Auth) }

// Prepare is the shared Search preamble every adapter runs before issuing its
// provider-specific request: it rejects a missing key or blank query, then
// normalizes the requested count into [ , max], substituting def for values
// below lo. label names the provider in the returned errors (e.g. "brave").
func (c *BaseClient) Prepare(label, query string, count, lo, def, max int) (int, error) {
	if c.APIKey() == "" {
		return 0, fmt.Errorf("%s: no API key configured", label)
	}
	if strings.TrimSpace(query) == "" {
		return 0, fmt.Errorf("%s: query is required", label)
	}
	if count < lo {
		count = def
	}
	if count > max {
		count = max
	}
	return count, nil
}
