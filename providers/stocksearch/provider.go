// Package stocksearch defines the pluggable interface for stock-photo
// search backends (Pexels, Unsplash, Pixabay, ...). Mirrors the imagegen
// package's shape: each backend lives in its own subpackage and self-
// registers a constructor in init(). The manage_stockphotos tool resolves
// a provider row from providers (kind='stock'), decrypts its key,
// then constructs a Provider via NewProvider.
package stocksearch

import "context"

// Provider is the interface every stock-search backend implements.
// Implementations hide vocabulary differences (Pexels' "large" vs
// Unsplash's "regular" vs Pixabay's URL buckets) behind a canonical
// PhotoSize enum, so the manage_stockphotos tool, and the LLM, can
// switch backends without learning a new vocabulary.
type Provider interface {
	// Name returns the display name of this provider instance (the row's
	// `name` from providers, used for logging and admin surfacing).
	Name() string

	// Type returns the provider type identifier ("pexels", "unsplash",
	// "pixabay"). Matches the registered constructor key.
	Type() string

	// Search runs a single keyword search and returns up to req.Count
	// results. Implementations must respect ctx cancellation.
	Search(ctx context.Context, req SearchRequest) (*SearchResponse, error)

	// Capabilities exposes static facts about the backend so the tool
	// layer can clamp counts and surface attribution requirements
	// up-front instead of at fetch time.
	Capabilities() Capabilities
}

// SearchRequest carries normalized search parameters.
type SearchRequest struct {
	Query       string
	Count       int
	Orientation string // "landscape" | "portrait" | "square" | "" (any)
	// PhotoSize is the canonical size token: "original", "large", "medium",
	// "small", "tiny". Each adapter maps these to its native vocabulary.
	// Empty string means "let the backend pick a sensible default", the
	// adapter implementations document what that resolves to.
	PhotoSize string
}

// SearchResult is one returned photo. URL is the size-resolved direct
// image URL ready to download, no secondary lookup required.
type SearchResult struct {
	URL          string
	Width        int
	Height       int
	Photographer string
	Alt          string
	// SourceID is the backend-side id, kept as string because Unsplash
	// uses slugs and Pixabay/Pexels use ints, string covers both.
	SourceID string
	// Attribution is a pre-formatted credit line ("Photo by X on Y") that
	// callers should append to alt text. Empty when the backend doesn't
	// require attribution (Pexels, Pixabay), set on Unsplash.
	Attribution string
}

// SearchResponse bundles the results.
type SearchResponse struct {
	Results []SearchResult
}

// Capabilities exposes what a backend can do. MaxCount caps the per-call
// count. Sizes is the canonical-token set the backend supports (informational
// , the tool doesn't gate on it; the adapter falls back to its closest
// match for unknown tokens).
type Capabilities struct {
	MaxCount            int
	Sizes               []string
	RequiresAttribution bool
}
