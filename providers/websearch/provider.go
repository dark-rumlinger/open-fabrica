// Package websearch defines the pluggable interface for real-time web
// search backends (Tavily, Brave, ...). Mirrors the stocksearch package's
// shape: each backend lives in its own subpackage and self-registers a
// constructor in init(). The manage_web_search tool resolves a provider row
// from providers (kind='search'), decrypts its key, then constructs a
// Provider via NewProvider.
//
// This gives the build/live LLM a way to ground answers in current
// information (latest library versions, recent news, today's docs) that
// predates or postdates the model's training cutoff, without depending on
// any single LLM vendor's server-side search feature.
package websearch

import "context"

// Provider is the interface every web-search backend implements. Backends
// hide vocabulary differences (Tavily's "topic"/"search_depth" vs Brave's
// "freshness"/"result_filter") behind the canonical SearchRequest so the
// manage_web_search tool, and the LLM, can switch backends without learning a
// new vocabulary.
type Provider interface {
	// Name returns the display name of this provider instance (the row's
	// `name` from providers, used for logging and admin surfacing).
	Name() string

	// Type returns the provider type identifier ("tavily", "brave").
	// Matches the registered constructor key.
	Type() string

	// Search runs a single query and returns up to req.Count results.
	// Implementations must respect ctx cancellation.
	Search(ctx context.Context, req SearchRequest) (*SearchResponse, error)

	// Capabilities exposes static facts about the backend so the tool
	// layer can clamp counts and surface feature support up-front.
	Capabilities() Capabilities
}

// SearchRequest carries normalized search parameters.
type SearchRequest struct {
	Query string
	Count int
	// Topic narrows the corpus: "news" for recent articles, "general" (or
	// "") for the open web. Backends that don't distinguish ignore it.
	Topic string
	// Freshness is a recency hint: "day", "week", "month", "year", or ""
	// (any). Each adapter maps these to its native parameter; backends
	// without a recency knob ignore it.
	Freshness string
}

// SearchResult is one returned hit. Snippet is the backend-supplied
// extract of the page content, already trimmed for LLM consumption.
type SearchResult struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published string `json:"published,omitempty"` // ISO date when the backend reports one
}

// SearchResponse bundles the results. Answer is an optional synthesized
// answer some backends (Tavily) return alongside the raw hits; empty when
// the backend doesn't produce one.
type SearchResponse struct {
	Results []SearchResult `json:"results"`
	Answer  string         `json:"answer,omitempty"`
}

// Capabilities exposes what a backend can do. MaxCount caps the per-call
// count. SupportsAnswer reports whether the backend returns a synthesized
// Answer. SupportsNews reports whether Topic="news" is honored.
type Capabilities struct {
	MaxCount       int
	SupportsAnswer bool
	SupportsNews   bool
}
