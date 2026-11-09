// Package imagegen defines the pluggable interface for AI image-generation
// backends (mirrors the llm package's shape). Each backend lives in its own
// subpackage and self-registers a constructor in init(), see imagegen/openai
// and imagegen/automatic1111.
package imagegen

import "context"

// Provider is the interface every image-generation backend implements.
// Implementations hide async-vs-sync behaviour: a Replicate-style poll loop
// or an A1111 local sync POST both look the same to the caller.
type Provider interface {
	// Name returns the display name of this provider instance (the row's
	// `name` from providers, used for logging and admin surfacing).
	Name() string

	// Type returns the provider type identifier ("openai-image",
	// "automatic1111", ...). Matches the registered constructor key.
	Type() string

	// Generate produces one or more images from a prompt. Implementations
	// must return raw bytes (not URLs) so callers don't have to know which
	// providers return URLs vs base64. Cancellation must honour ctx.
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)

	// Capabilities exposes static facts about the backend so the tool layer
	// can short-circuit unsupported requests with a clean error before
	// hitting the network.
	Capabilities() Capabilities
}

// GenerateRequest carries normalized image-generation parameters. Provider-
// specific knobs (sampler, steps, cfg_scale, lora) ride in Extra so the
// shared shape doesn't bloat with every backend's options.
type GenerateRequest struct {
	Prompt   string
	Negative string // ignored by providers that don't support it
	Model    string // provider-scoped model id; empty = provider default
	Size     string // "1024x1024", "512x768", or "auto"
	N        int    // number of images; clamped to Capabilities.MaxN
	Seed     *int64 // nil = provider chooses
	Quality  string // "low" | "standard" | "high"; provider maps to its own scale
	Extra    map[string]any
}

// GeneratedImage is a single produced image. ContentType is set by the
// provider (PNG for OpenAI/A1111 by default). Seed is the actual seed used
// , useful when the request specified Seed=nil and the user later wants to
// reproduce the result.
type GeneratedImage struct {
	Bytes       []byte
	ContentType string
	Seed        int64
	// Revised carries any prompt rewrite the backend performed (OpenAI's
	// gpt-image-1 returns a revised_prompt field when its safety/style
	// system rewords the input). Empty when the backend doesn't expose it.
	Revised string
}

// GenerateResponse bundles the produced images.
type GenerateResponse struct {
	Images []GeneratedImage
}

// ModelLister is an optional interface for image backends whose model
// catalog can be discovered at runtime (OpenAI's /v1/models, A1111's
// /sdapi/v1/sd-models). Mirrors llm.ModelLister, kept in this package so
// imagegen has no dependency on llm. The admin UI offers a "Sync Models"
// button when the provider implements it.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ModelInfo is the discovery-time shape for an image model. Narrower than
// llm.ModelInfo since context windows / streaming / tool-use don't apply
// to image generation. The admin sync code maps this onto the shared
// llm_models row via SyncProviderModels.
type ModelInfo struct {
	ID          string // canonical model id passed to Generate (gpt-image-2, sd_xl_base_1.0, ...)
	DisplayName string // human label for pickers
}

// Capabilities exposes what a backend can do. Used by the tool layer to
// validate requests up front and to filter the model list surfaced to the
// LLM. Conservative defaults are fine, better to under-promise than to
// surface a knob that silently no-ops.
type Capabilities struct {
	MaxN             int
	Sizes            []string // e.g. ["1024x1024", "1024x1792", "1792x1024"]
	SupportsNegative bool
	SupportsSeed     bool
	SupportsImg2Img  bool
}
