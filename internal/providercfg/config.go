// Package providercfg is the typed decoding of the free-form `config` JSON
// column on `providers` (and of the matching "config" object in seed.json).
//
// It exists so a provider's behaviour is DATA rather than a Go package. Adding
// a vendor that speaks an already-implemented wire protocol should be one seed
// row or one admin form submission, never a new transport: the fields here are
// the knobs that would otherwise require a wrapper type (attribution headers,
// explicit prompt-cache breakpoints, a listing URL on a different path, a listing
// payload in a different shape).
//
// The line this package draws: request SHAPING and auth are data; response
// PARSING stays code, selected by a named ModelsShape. Parsers stay typed and
// testable, and a malformed shape id fails at boot rather than at first use.
package providercfg

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Auth scheme identifiers. Closed set: Validate rejects anything else, so a
// typo in seed.json fails startup rather than silently sending no credential.
const (
	// SchemeBearer sends "Authorization: Bearer <key>". The default.
	SchemeBearer = "bearer"
	// SchemeHeader sends "<Header>: <Prefix><key>" - covers Anthropic's
	// x-api-key, Brave's X-Subscription-Token, Pexels' bare Authorization,
	// and Unsplash's "Authorization: Client-ID <key>".
	SchemeHeader = "header"
	// SchemeQuery appends "?<Param>=<key>" - Pixabay, and the Gemini native
	// models listing.
	SchemeQuery = "query"
	// SchemeBody puts the key in the JSON request body under Param. The
	// transport owns this one: a body is built from a typed struct, so
	// httpauth.Apply cannot inject it. Tavily is the only user.
	SchemeBody = "body"
	// SchemeNone sends no credential at all - keyless local runtimes.
	SchemeNone = "none"
)

// Model-listing payload shapes. Closed set, same reasoning as the auth schemes.
//
// These name the shapes the openai-chat transport can parse; a transport that
// owns a single proprietary catalog format (anthropic) parses its own and
// ignores ModelsShape entirely.
const (
	// ShapeOpenAI is GET /models -> {"data":[{"id":...}]}.
	ShapeOpenAI = "openai"
	// ShapeOpenRouter is the same envelope carrying OpenRouter's richer
	// per-model metadata: context_length, architecture.input_modalities,
	// and supported_parameters.
	ShapeOpenRouter = "openrouter"
	// ShapeGemini is GET /v1beta/models -> {"models":[{"name":"models/x"}]},
	// filtered to entries declaring generateContent.
	ShapeGemini = "gemini"
	// ShapeOllama is GET /api/tags -> {"models":[{"name":"llama3:8b"}]}.
	ShapeOllama = "ollama"
	// ShapeLlamaCpp is the OpenAI envelope plus meta.n_ctx_train, refined by
	// a second /props fetch for the runtime -c window.
	ShapeLlamaCpp = "llamacpp"
)

// Auth describes how a credential is attached to an outbound request. The zero
// value means bearer, which is what the overwhelming majority of providers
// want; only the exceptions carry an explicit descriptor.
type Auth struct {
	// Scheme is one of the Scheme* constants. Empty means SchemeBearer.
	Scheme string `json:"scheme,omitempty"`
	// Header is the header name for SchemeHeader (e.g. "x-api-key").
	Header string `json:"header,omitempty"`
	// Prefix is prepended to the key for SchemeHeader (e.g. "Client-ID ").
	// Include the trailing space if the vendor expects one.
	Prefix string `json:"prefix,omitempty"`
	// Param is the query-string or body key for SchemeQuery / SchemeBody.
	Param string `json:"param,omitempty"`
}

// Effective returns the scheme with the zero value resolved to bearer, so
// callers can switch on the result without repeating the default.
func (a Auth) Effective() string {
	if a.Scheme == "" {
		return SchemeBearer
	}
	return a.Scheme
}

// Config is the decoded `providers.config` blob.
type Config struct {
	// Auth describes how the API key is attached to normal requests.
	Auth Auth `json:"auth,omitzero"`
	// ModelsAuth overrides Auth for the model-listing request only. Gemini
	// needs it: its OpenAI-compatible chat surface takes a bearer token
	// while its native /v1beta/models listing takes ?key=.
	ModelsAuth *Auth `json:"models_auth,omitempty"`
	// ExtraHeaders are set on every request. Static vendor requirements
	// (Anthropic's anthropic-version, OpenRouter's HTTP-Referer/X-Title,
	// Unsplash's Accept-Version) live here instead of in a wrapper type.
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`
	// CacheControl enables explicit cache_control breakpoints on the
	// openai-chat transport. Only gateways that honour them want this
	// (OpenRouter routing to Anthropic/Gemini); automatic-cache endpoints
	// must leave it off, where a breakpoint is a no-op and the
	// array-of-parts content it forces can disturb their prefix matcher.
	CacheControl bool `json:"cache_control,omitempty"`
	// CacheTTL selects the prompt-cache entry lifetime on transports with
	// explicit breakpoints (anthropic): "1h" buys the extended cache (2x
	// write premium, same read discount) when calls can sit more than 5
	// minutes apart. Empty means the provider default (5m).
	CacheTTL string `json:"cache_ttl,omitempty"`
	// ModelsShape selects the listing response parser. Empty means ShapeOpenAI.
	ModelsShape string `json:"models_shape,omitempty"`
	// EmbeddingModel is the model id used for embedding requests. Empty means
	// this provider serves no embeddings; callers must surface a clear error
	// rather than fall back to a hardcoded model.
	EmbeddingModel string `json:"embedding_model,omitempty"`
	// StrictModelFilter restricts a listing to the OpenAI first-party families
	// (gpt-/chatgpt-/o-series). Only api.openai.com wants it; every other
	// vendor on the openai-chat transport has its own naming scheme and would
	// be filtered down to nothing.
	StrictModelFilter bool `json:"strict_model_filter,omitempty"`
}

// EffectiveShape returns ModelsShape with the zero value resolved to openai.
func (c Config) EffectiveShape() string {
	if c.ModelsShape == "" {
		return ShapeOpenAI
	}
	return c.ModelsShape
}

// ListingAuth returns the auth descriptor for the model-listing request:
// ModelsAuth when set, otherwise the normal Auth.
func (c Config) ListingAuth() Auth {
	if c.ModelsAuth != nil {
		return *c.ModelsAuth
	}
	return c.Auth
}

// IsZero reports whether the descriptor states nothing at all, which is how a
// row says "use whatever this transport normally does".
func (a Auth) IsZero() bool { return a == Auth{} }

// WithDefaults layers a transport's own vendor knowledge under the row's
// config: the row wins wherever it states something, the transport fills the
// rest.
//
// This is the shape that keeps ONE source of truth per provider. A transport
// that exists to parse a specific vendor (pexels, brave, anthropic) already
// knows that vendor's credential header and required version pins, so a plain
// seed row needs no auth config at all - and seed.json restating it would be a
// second copy free to drift. An operator fronting that vendor with a proxy
// that wants something else still overrides it from config, with no code
// change. Generic transports pass a zero Auth and get bearer.
func (c Config) WithDefaults(auth Auth, extra map[string]string) Config {
	if c.Auth.IsZero() {
		c.Auth = auth
	}
	if len(extra) == 0 {
		return c
	}
	merged := make(map[string]string, len(extra)+len(c.ExtraHeaders))
	for k, v := range extra {
		merged[k] = v
	}
	for k, v := range c.ExtraHeaders { // row wins
		merged[k] = v
	}
	c.ExtraHeaders = merged
	return c
}

var (
	validSchemes = map[string]bool{
		SchemeBearer: true, SchemeHeader: true, SchemeQuery: true,
		SchemeBody: true, SchemeNone: true,
	}
	validShapes = map[string]bool{
		ShapeOpenAI: true, ShapeOpenRouter: true, ShapeGemini: true,
		ShapeOllama: true, ShapeLlamaCpp: true,
	}
)

// Parse decodes a `providers.config` blob. An empty or malformed blob yields
// the zero Config rather than an error: config is advisory decoration on a row
// that is otherwise usable, and a provider must not become unreachable because
// its options JSON got mangled. Use ParseStrict at boundaries that should
// reject bad input (seed loading, the admin form).
func Parse(blob string) Config {
	cfg, err := ParseStrict(blob)
	if err != nil {
		return Config{}
	}
	return cfg
}

// ParseStrict decodes and validates. Returns an error on malformed JSON, an
// unknown auth scheme, an unknown models shape, or a scheme missing the field
// it needs.
func ParseStrict(blob string) (Config, error) {
	var cfg Config
	if strings.TrimSpace(blob) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
		return Config{}, fmt.Errorf("provider config: %w", err)
	}
	return cfg, cfg.Validate()
}

// Validate checks the closed sets and the per-scheme required fields.
func (c Config) Validate() error {
	if err := c.Auth.validate("auth"); err != nil {
		return err
	}
	if c.ModelsAuth != nil {
		if err := c.ModelsAuth.validate("models_auth"); err != nil {
			return err
		}
	}
	if s := c.EffectiveShape(); !validShapes[s] {
		return fmt.Errorf("provider config: unknown models_shape %q (want one of %s)", s, sortedKeys(validShapes))
	}
	return nil
}

func (a Auth) validate(field string) error {
	s := a.Effective()
	if !validSchemes[s] {
		return fmt.Errorf("provider config: %s: unknown scheme %q (want one of %s)", field, s, sortedKeys(validSchemes))
	}
	if s == SchemeHeader && a.Header == "" {
		return fmt.Errorf("provider config: %s: scheme %q requires a header name", field, s)
	}
	if (s == SchemeQuery || s == SchemeBody) && a.Param == "" {
		return fmt.Errorf("provider config: %s: scheme %q requires a param name", field, s)
	}
	return nil
}

// Marshal renders the config back to JSON for storage.
func (c Config) Marshal() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("provider config: %w", err)
	}
	return string(b), nil
}

func sortedKeys(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
