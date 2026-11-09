package llm

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// DefaultHTTPTimeout is the whole-request ceiling installed on a provider's
// HTTP client when the caller passes none. One shared constant so every LLM
// transport (anthropic, openai-chat, ollama) fails a hung call at the same
// point; callers needing longer runs pass their own *http.Client.
const DefaultHTTPTimeout = 6 * time.Minute

// EphemeralCacheControl is the cache_control value that marks a prompt-cache
// breakpoint, shared by every transport that stamps explicit breakpoints
// (anthropic natively, openai-format gateways via emitCacheControl) so the
// stamp sites can't drift from one another.
func EphemeralCacheControl() map[string]string {
	return map[string]string{"type": "ephemeral"}
}

// EphemeralCacheControlTTL is EphemeralCacheControl with an explicit entry
// lifetime: "1h" buys the extended cache (2x write premium, same read
// discount) for workloads whose calls can sit more than 5 minutes apart.
// Empty ttl returns the plain form.
func EphemeralCacheControlTTL(ttl string) map[string]string {
	cc := EphemeralCacheControl()
	if ttl != "" {
		cc["ttl"] = ttl
	}
	return cc
}

// CacheBreakpointIndices returns the indices into msgs of the Cacheable user
// messages that should carry an explicit cache breakpoint: the tail-most
// budget of them, since a later breakpoint's prefix already covers earlier
// ones. Shared by every transport that stamps breakpoints; each caller
// computes its own budget (at most 4 total per request, minus whatever it
// reserves for system/tool/final-message blocks) and may pass a prefix slice
// of its messages to keep a reserved message out of candidacy - a prefix
// keeps indices aligned with the full slice. A budget of zero or less
// selects nothing.
func CacheBreakpointIndices(msgs []Message, budget int) map[int]bool {
	out := map[int]bool{}
	if budget <= 0 {
		return out
	}
	var idxs []int
	for i, m := range msgs {
		if m.Cacheable && m.Role == RoleUser {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) > budget {
		idxs = idxs[len(idxs)-budget:]
	}
	for _, i := range idxs {
		out[i] = true
	}
	return out
}

// Provider is the interface that LLM providers must implement.
type Provider interface {
	// Name returns the display name of this provider instance.
	Name() string

	// Type returns the provider type identifier (e.g. "anthropic", "openai").
	Type() string

	// Complete sends a completion request and returns the full response.
	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)

	// Stream sends a completion request and streams the response via callback.
	Stream(ctx context.Context, req CompletionRequest, callback StreamCallback) error

	// Ping checks connectivity to the provider using the given model ID.
	Ping(ctx context.Context, model string) error
}

// EmbeddingProvider is an optional interface for providers that support text embeddings.
// Used for vector/semantic search features.
type EmbeddingProvider interface {
	// Embed generates embedding vectors for the given text inputs.
	Embed(ctx context.Context, texts []string, model string) ([][]float32, error)
}

// StructuredOutputProvider is an optional interface for providers that can
// constrain a completion response to a JSON schema. When the engine pipeline
// needs a JSON-typed payload (e.g. PLAN) it sets ResponseSchema on the
// CompletionRequest; providers implementing this interface honour it natively
// instead of relying on free-form text post-processing.
type StructuredOutputProvider interface {
	SupportsStructuredOutput() bool
}

// ContextWindowProvider is implemented by a provider that knows, for certain, how
// many tokens one request may hold: a local runtime whose window the operator set
// explicitly. A provider that can only guess (a hosted model's catalog entry, a
// name heuristic) does not implement it, so nothing keys on a guess.
type ContextWindowProvider interface {
	ContextWindow() int
}

// ModelLister is an optional interface for providers whose model catalog can
// be discovered at runtime (OpenRouter, Ollama, …). Implementing this lets
// the admin UI offer a "Sync models" button instead of forcing the user to
// hand-add every row. Providers that don't implement it just keep the
// manual-add flow, the admin endpoint surfaces a clear error.
type ModelLister interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
}

// ModelInfo is the discovery-time shape. Every backend must be able to fill it
// without a lossy conversion, so a field only exists here if some provider
// actually reports it; the rest is filled from a name heuristic afterwards
// (see FillFromName). The DB row layer merges this with user-set fields
// (is_enabled, is_default).
type ModelInfo struct {
	ID          string // canonical model id sent in completion requests
	DisplayName string // human-readable label for pickers
	// ContextTokens is the total prompt+completion window. MaxOutputTokens is
	// the per-response cap. Two fields rather than one because vendors report
	// different ones under the same name: Anthropic's /v1/models max_tokens is
	// the output cap, OpenRouter's context_length is the window. Conflating
	// them is what made the old single column unreadable. 0 = unknown.
	ContextTokens     int
	MaxOutputTokens   int
	SupportsStreaming bool
	SupportsTools     bool
	SupportsVision    bool
	SupportsReasoning bool
	// Cost is USD per million tokens; zero when unknown.
	Cost ModelCost
}

// ModelCost is a model's published pricing, USD per MILLION tokens.
type ModelCost struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// ResponseSchema constrains a completion response to a JSON value matching
// the given JSON-schema. Each provider maps it onto its native mechanism
// (OpenAI-compatible response_format=json_schema, Anthropic forced single-tool,
// Ollama format). A provider that cannot honor a strict JSON-schema response
// must surface a typed Error{Code:CodeSchemaUnsupported} rather than silently
// falling back to free text.
type ResponseSchema struct {
	Name   string          // identifier used by providers that name the schema (OpenAI, Anthropic tool name)
	Schema json.RawMessage // the JSON Schema document
	Strict bool            // when true, providers must reject extra properties
}

// CompletionRequest is the input to a completion call.
type CompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	System         string          `json:"system,omitempty"`
	Tools          []ToolDef       `json:"tools,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	StopWords      []string        `json:"stop,omitempty"`
	CacheSystem    bool            `json:"-"` // Anthropic prompt caching: cache system prompt + tools
	ResponseSchema *ResponseSchema `json:"-"` // when set, response Content must parse against this JSON schema

	// PromptCacheKey is an optional routing hint for providers with automatic
	// prefix caching (OpenAI's `prompt_cache_key`). Requests sharing a key are
	// biased to the same cache node so a build's sequential same-prefix calls
	// hit a warm cache instead of cold-starting on a different shard. Empty
	// means "send no hint". Providers that don't support it ignore the field
	// (unknown JSON keys are dropped); the Anthropic client never reads it.
	PromptCacheKey string `json:"-"`

	// Prefix-stability diagnostics, populated by callers that track how much of
	// this request's serialized prefix is byte-identical to the previous request
	// in the same session - the property every provider prompt cache keys on.
	// Providers never read them; LoggedProvider copies them into the LLM log so
	// cache regressions are visible next to the provider-reported cache counters.
	StablePrefixBytes int `json:"-"`
	StablePrefixMsgs  int `json:"-"`
	TotalRequestBytes int `json:"-"`

	// Thinking, when non-nil, explicitly enables or disables extended-reasoning
	// mode on providers that expose a toggle. Nil means "don't send any toggle;
	// let the provider use its default."
	//
	// Why this exists: reasoning models stream chain-of-thought tokens into a
	// separate `reasoning_content` channel that bills against the output
	// budget but never surfaces in `content` or `tool_calls`. On budget-bound
	// stages a reasoning model can spend the entire output budget on hidden
	// thought and return an empty response with finish_reason=length. Setting
	// Thinking=false on those stages keeps the model in direct-response mode
	// and avoids that failure mode.
	Thinking *bool `json:"-"`

	// Effort, when non-empty, sets how much the model deliberates and how many
	// tokens it spends per turn ("low" | "medium" | "high" | "xhigh" | "max").
	// It is the primary per-turn latency lever on models that expose it - the
	// newer Anthropic models default to "high" (the slowest), so leaving this
	// empty runs them at their most deliberate. Providers that don't support an
	// effort control ignore the field. Empty means "send nothing; use the
	// provider/model default."
	Effort string `json:"-"`
}

// CompletionResponse is the output from a completion call.
type CompletionResponse struct {
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	Usage      Usage      `json:"usage"`
	Model      string     `json:"model"`
	StopReason string     `json:"stop_reason,omitempty"`

	// ReasoningContent carries any chain-of-thought tokens the provider emitted
	// in a separate reasoning channel (Z.ai/DeepSeek `reasoning_content`,
	// xAI/OpenAI-style `reasoning`). Populated only by providers that
	// surface a reasoning channel; empty for everyone else. Callers should
	// treat this as diagnostic context, not as a substitute for Content -
	// the model's actual answer (visible text or tool_calls) is still in
	// Content / ToolCalls.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Error codes classify provider failures so the retry layer can route them
// without scraping error messages. Provider clients build *Error from HTTP
// status + body and wrap any underlying transport error in Unwrap.
const (
	CodeRateLimit         = "rate_limit"         // 429, 529, "overloaded" (transient per-window throttle)
	CodeQuotaExceeded     = "quota_exceeded"     // 429 with an account/plan quota that resets hours away
	CodeTimeout           = "timeout"            // context deadline / network timeout
	CodeOverloaded        = "overloaded"         // provider-reported temporary overload (distinct from 429)
	CodeAuth              = "auth"               // 401 / 403
	CodeBadRequest        = "bad_request"        // 400 (schema invalid, params wrong, etc.)
	CodeContextOverflow   = "context_overflow"   // 400/413 because the prompt exceeds the model's context window
	CodeSchemaUnsupported = "schema_unsupported" // provider/model cannot honour ResponseSchema
	CodeTransport         = "transport"          // DNS / TLS / connection refused
	CodeServer            = "server"             // generic 5xx (retryable)
	CodeTruncated         = "truncated"          // provider returned stop_reason=max_tokens / length
	CodeSchemaDrift       = "schema_drift"       // 200 OK but the output ignored the structured-output schema (not valid JSON)
)

// Error is the typed shape every provider client returns from a failing
// completion. Callers should use errors.As to extract it; matching on the
// string form is forbidden.
type Error struct {
	Code       string        // one of the Code* constants above
	Status     int           // HTTP status, 0 when not applicable
	Message    string        // raw provider-side detail
	RetryAfter time.Duration // honour-after hint from headers / body; 0 when not provided
	Cause      error         // underlying transport error, if any
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Cause }

// AsLLMError returns the typed *Error inside err, if any.
func AsLLMError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
