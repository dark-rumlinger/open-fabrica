package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"openfabrica/internal/providercfg"
	"openfabrica/providers/internal/httpauth"
	"openfabrica/providers/llm"
)

// defaultBaseURL is an API ROOT, not an operation endpoint. Every provider base
// URL in the system names the root and the transport appends the path, so a
// gateway that mounts chat somewhere unexpected is still reachable and the
// sibling paths (/models, /embeddings) never have to be recovered by trimming
// a suffix that may not be there.
const defaultBaseURL = "https://api.openai.com/v1"

// Client implements llm.Provider for the OpenAI-compatible API - the wire
// protocol the large majority of vendors speak. Every compatible endpoint is
// THIS transport plus a config row; none of them needs a Go package.
type Client struct {
	name       string
	apiKey     string
	endpoint   string // chat completions
	modelsURL  string // catalog listing
	embedURL   string // embeddings
	propsURL   string // llama-server /props, empty for everyone else
	httpClient *http.Client
	cfg        providercfg.Config

	// emitCacheControl turns on explicit cache_control:ephemeral breakpoints in the
	// OpenAI content-part format. Off by default: only gateways with explicit prompt
	// caching set it (OpenRouter, via SetCacheControl). Automatic-cache endpoints leave
	// it off, where a breakpoint is a no-op and the array-of-parts content it forces
	// can disturb their prefix matcher; they use string content plus prompt_cache_key.
	emitCacheControl bool
}

// transportID is both the registry key and the id this client reports. It names
// the WIRE PROTOCOL, not the vendor: two vendor rows on this protocol are both
// openai-chat, and they are told apart by providers.name, which is what the
// capability store keys on and what the call log records alongside the type.
const transportID = "openai-chat"

// CapKey returns the capability-store key for this client and model. Keyed on
// the provider ROW name rather than the transport, so two vendors sharing this
// transport learn their quirks independently.
func (c *Client) CapKey(model string) llm.ModelKey {
	return llm.ModelKey{Provider: c.name, Model: model}
}

func init() {
	llm.RegisterProvider(transportID, func(o llm.Options) llm.Provider {
		return New(o)
	})
}

// New creates a new OpenAI-compatible provider. Options.BaseURL is optional
// (empty uses api.openai.com) and is an API root; Options.HTTPClient is
// optional (nil installs a 6-minute timeout client).
func New(o llm.Options) *Client {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: llm.DefaultHTTPTimeout}
	}
	modelsURL := o.ModelsURL
	if modelsURL == "" {
		modelsURL = o.Endpoint(defaultBaseURL, "models")
	}
	c := &Client{
		name:             o.Name,
		apiKey:           o.APIKey,
		endpoint:         o.Endpoint(defaultBaseURL, "chat", "completions"),
		modelsURL:        modelsURL,
		embedURL:         o.Endpoint(defaultBaseURL, "embeddings"),
		httpClient:       hc,
		cfg:              o.Config,
		emitCacheControl: o.Config.CacheControl,
	}
	// llama-server reports the runtime context window (-c) at /props, a sibling
	// of the /v1 surface rather than a child of it. Resolved here so the listing
	// parser never has to reason about the base URL's shape.
	if o.Config.EffectiveShape() == providercfg.ShapeLlamaCpp {
		c.propsURL = o.Sibling(defaultBaseURL, "props")
	}
	return c
}

func (c *Client) Name() string { return c.name }
func (c *Client) Type() string { return transportID }

// SupportsStructuredOutput reports that the OpenAI chat endpoint supports
// response_format=json_schema. This is a claim about the TRANSPORT, and every
// wrapper embedding this client inherits it (only ollama overrides). A model
// that turns out not to honour it is caught at runtime: Complete's degrade
// ladder demotes CapStructuredOutput on the first schema rejection.
func (c *Client) SupportsStructuredOutput() bool {
	// api.openai.com supports strict json_schema since gpt-4o-2024-08-06; assume any
	// caller pointing at this transport with a compatible model supports it too.
	return true
}

// BaseURL returns the chat-completions endpoint this client was constructed with.
// Lets wrapper providers (Ollama, Gemini) derive a sibling URL (e.g. /api/tags)
// without re-parsing the caller-supplied base_url.
func (c *Client) BaseURL() string { return c.endpoint }

// APIKey returns the bearer token this client uses. Exported so wrapper providers
// can authenticate their own listing endpoints with the same credential.
func (c *Client) APIKey() string { return c.apiKey }

// HTTPClient returns the underlying *http.Client so wrapper providers (Ollama,
// Gemini) can reuse the same transport for their ListModels requests.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// ListModels fetches the provider's catalog and parses it according to the
// row's models_shape. Every vendor sharing this transport lists differently
// enough to need a parser but not differently enough to need a transport, so
// the parsers live together in llm/modellist.go and are selected by config.
func (c *Client) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	return llm.ListModels(ctx, llm.ModelListSpec{
		HTTPClient:           c.httpClient,
		URL:                  c.modelsURL,
		Shape:                c.cfg.EffectiveShape(),
		Label:                c.name,
		SetHeaders:           c.setListingHeaders,
		StrictOpenAIFamilies: c.cfg.StrictModelFilter,
		RuntimeInfoURL:       c.propsURL,
	})
}

// Ping checks connectivity by sending a minimal completion request.
func (c *Client) Ping(ctx context.Context, model string) error {
	req := llm.CompletionRequest{
		Model:     model,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "ping"}},
		MaxTokens: 1,
	}
	_, err := c.Complete(ctx, req)
	return err
}

// Complete sends a non-streaming completion request.
//
// A rejected request-shaping field is learned once per model and the request
// re-rendered without it (see providers/llm/model_caps_learn.go).
//
// When the request pins a json_schema response_format and the endpoint rejects it,
// Complete degrades stepwise and retries: json_schema -> json_object -> none. That
// ladder is a payload degrade rather than a capability, so it runs after the
// quirk loop rather than inside it.
func (c *Client) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	var body map[string]any

	apiResp, err := llm.RetryOnQuirk(transportID, c.CapKey(req.Model), req, func() (*apiResponse, error) {
		// Re-render rather than patch the map: buildRequestBody reads the store,
		// so one code path decides what a degraded body looks like.
		return c.postChat(ctx, c.buildRequestBody(req, false))
	})
	if err != nil && req.ResponseSchema != nil && isJSONSchemaRejection(err) {
		// Record that this model cannot be held to a schema, so the engine's
		// provider fallback fires on the first call next session instead of
		// re-paying the whole ladder every time.
		llm.Caps.Learn(c.CapKey(req.Model), llm.CapStructuredOutput, llm.No)
		body = c.buildRequestBody(req, false)
		// degradeFormats are the looser response_format modes to fall back
		// through, in order. A nil entry drops the field entirely.
		degradeFormats := []map[string]any{
			{"type": "json_object"},
			nil,
		}
		for _, format := range degradeFormats {
			if format == nil {
				delete(body, "response_format")
			} else {
				body["response_format"] = format
			}
			apiResp, err = c.postChat(ctx, body)
			if err == nil || !isJSONSchemaRejection(err) {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}

	return c.parseResponse(apiResp), nil
}

// postChat marshals body, POSTs it to the chat-completions endpoint, and decodes
// the reply. HTTP and transport failures come back as typed *llm.Error so both
// the retry layer and Complete's json-mode fallback can route on the code.
// Errors are tagged with the provider ROW name, which is unique and names the
// vendor an operator actually configured - far more useful in a log line than
// the wire protocol, which most vendors share.
func (c *Client) postChat(ctx context.Context, body map[string]any) (*apiResponse, error) {
	var apiResp apiResponse
	if err := llm.PostJSON(ctx, c.httpClient, c.endpoint, c.name, body, c.setHeaders, &apiResp); err != nil {
		return nil, err
	}
	return &apiResp, nil
}

// isJSONSchemaRejection reports whether err is the endpoint refusing a
// response_format=json_schema request (a 400 classified as CodeSchemaUnsupported),
// so Complete can decide whether a json_object retry is worth attempting.
func isJSONSchemaRejection(err error) bool {
	lerr, ok := llm.AsLLMError(err)
	return ok && lerr.Code == llm.CodeSchemaUnsupported
}

// Stream sends a streaming completion request and calls the callback for each chunk.
// The same learn-and-retry as Complete applies, and is safe here because a rejected
// field comes back as a 400 classified inside openStream, before any SSE byte
// reaches the callback.
func (c *Client) Stream(ctx context.Context, req llm.CompletionRequest, callback llm.StreamCallback) error {
	resp, err := llm.RetryOnQuirk(transportID, c.CapKey(req.Model), req, func() (*http.Response, error) {
		// Re-render per attempt; the open 200 response is owned by this caller.
		return llm.PostJSONStream(ctx, c.httpClient, c.endpoint, c.name, c.buildRequestBody(req, true), c.setHeaders)
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return c.parseSSEStream(resp.Body, callback)
}

// setHeaders sets common headers for OpenAI-compatible API requests. The
// credential and any static per-vendor headers come from the provider row's
// config, which is how OpenRouter's attribution pair (HTTP-Referer, X-Title)
// stopped needing a decorating RoundTripper in a package of its own.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	httpauth.Apply(req, c.cfg.Auth, c.apiKey)
	httpauth.SetExtra(req, c.cfg.ExtraHeaders)
}

// setListingHeaders authenticates a catalog request. Split from setHeaders
// because a provider can want a different credential placement for listing
// than for chat - Gemini takes a bearer token on its OpenAI-compatible chat
// surface but ?key= on its native /v1beta/models.
func (c *Client) setListingHeaders(req *http.Request) {
	httpauth.Apply(req, c.cfg.ListingAuth(), c.apiKey)
	httpauth.SetExtra(req, c.cfg.ExtraHeaders)
}

// buildRequestBody converts our generic request into the OpenAI API format.
func (c *Client) buildRequestBody(req llm.CompletionRequest, stream bool) map[string]any {
	body := map[string]any{
		"model": req.Model,
	}
	// Every capability read below goes through this key, so the whole layer stays
	// visible in one function rather than spreading across the transport.
	k := c.CapKey(req.Model)
	// Learned-only: a declared supports_vision=0 usually means the discovery
	// heuristic did not recognise the model name, not that it cannot see. Acting
	// on that would silently strip images from capable models.
	if llm.Caps.Learned(k, llm.CapVision) == llm.No {
		req, _ = llm.StripImages(req)
	}

	body["messages"] = c.buildMessages(req)

	if req.MaxTokens > 0 {
		// Reasoning models reject max_tokens and want max_completion_tokens; the
		// first 400 teaches the store which one this model takes.
		if llm.Caps.Allowed(k, llm.CapMaxTokensField) {
			body["max_tokens"] = req.MaxTokens
		} else {
			body["max_completion_tokens"] = req.MaxTokens
		}
	}
	if llm.Caps.Allowed(k, llm.CapSamplingParams) {
		if req.Temperature != nil {
			body["temperature"] = *req.Temperature
		}
		if req.TopP != nil {
			body["top_p"] = *req.TopP
		}
	}
	if len(req.StopWords) > 0 {
		body["stop"] = req.StopWords
	}

	// Cache-routing hint for providers with automatic prefix caching. OpenAI uses
	// prompt_cache_key to bias same-key requests to the same cache node so a build's
	// sequential same-prefix calls warm-hit instead of cold-starting on another shard.
	// Endpoints that don't recognise it ignore the field.
	if req.PromptCacheKey != "" {
		body["prompt_cache_key"] = req.PromptCacheKey
	}

	// Tools
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			if t.Type == "web_search" {
				// OpenAI uses web_search_options as a top-level parameter
				// (works with search models like gpt-4o-search-preview, gpt-5-search-api).
				body["web_search_options"] = map[string]any{}
				continue
			}
			tool := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			body["tools"] = tools
			// Ask for multi-call turns explicitly. OpenAI defaults this on, but
			// several compatible gateways default it off, forcing an agent into
			// one-tool-call-per-turn micro-iterations that re-pay the full cached
			// prefix. Dropped for models whose endpoint has 400d on the field.
			if llm.Caps.Allowed(k, llm.CapParallelToolCalls) {
				body["parallel_tool_calls"] = true
			}
		}
	}

	if stream {
		body["stream"] = true
		// Without include_usage, OpenAI streams emit no token counts, leaving the
		// engine blind to per-iteration cost. It costs nothing (one extra final chunk).
		body["stream_options"] = map[string]any{
			"include_usage": true,
		}
	}

	applyReasoningMode(body, req.Thinking, k)

	if req.ResponseSchema != nil {
		var schema any
		if err := json.Unmarshal(req.ResponseSchema.Schema, &schema); err == nil {
			name := req.ResponseSchema.Name
			if name == "" {
				name = "response"
			}
			body["response_format"] = map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   name,
					"schema": schema,
					"strict": req.ResponseSchema.Strict,
				},
			}
		}
	}

	return body
}

// applyReasoningMode sets the reasoning-mode toggle for Z.ai GLM, DeepSeek-R1,
// Qwen-thinking, and the OpenRouter passthrough of any of these. Both shapes
// (thinking.type and enable_thinking) are emitted so the same flag works across
// gateways, and they always agree. nil leaves provider defaults alone. A model
// whose endpoint refuses the disabled mode gets thinking enabled at the cheapest
// legal level instead, as close to the caller's "off" as the endpoint permits.
func applyReasoningMode(body map[string]any, thinking *bool, k llm.ModelKey) {
	switch {
	case thinking == nil:
		// No opinion: leave provider defaults alone.
	case !llm.Caps.Allowed(k, llm.CapThinkingField):
		// The endpoint 400s on the toggle field itself (strict OpenAI-style
		// "unknown parameter"): omit it in both directions.
	case *thinking:
		body["thinking"] = map[string]any{"type": "enabled"}
		body["enable_thinking"] = true
	case llm.Caps.Allowed(k, llm.CapThinkingOff):
		body["thinking"] = map[string]any{"type": "disabled"} // Z.ai canonical
		body["enable_thinking"] = false                       // vLLM / OpenViking / OpenRouter shim
	default:
		body["thinking"] = map[string]any{"type": "enabled"}
		body["enable_thinking"] = true
		if llm.Caps.Allowed(k, llm.CapReasoningEffort) {
			body["reasoning_effort"] = forcedReasoningEffort
		}
	}
}

// buildMessages renders the request into the OpenAI messages array: the system
// prompt, role mapping, assistant tool_calls, multimodal user parts, and the
// explicit cache-control breakpoints (all no-ops unless emitCacheControl is set,
// so the default openai-chat path is unchanged). Image stripping for non-vision
// models happens in buildRequestBody before this is called.
func (c *Client) buildMessages(req llm.CompletionRequest) []map[string]any {
	var msgs []map[string]any

	// Which Cacheable user messages get an explicit breakpoint this request.
	cacheableUserIdx, stampFinal := c.cacheBreakpoints(req)

	// If there's a system prompt, add it as a system message. With explicit caching
	// on, the system block becomes a content-part array carrying the first breakpoint,
	// matching the Anthropic client over the OpenAI-compatible wire.
	if req.System != "" {
		sysMsg := map[string]any{"role": "system"}
		if c.emitCacheControl && req.CacheSystem {
			sysMsg["content"] = []map[string]any{
				{"type": "text", "text": req.System, "cache_control": llm.EphemeralCacheControlTTL(c.cfg.CacheTTL)},
			}
		} else {
			sysMsg["content"] = req.System
		}
		msgs = append(msgs, sysMsg)
	}

	for mi, m := range req.Messages {
		msg := map[string]any{
			"role": string(m.Role),
		}

		if m.Role == llm.RoleTool {
			// Tool result message
			msg["role"] = "tool"
			msg["content"] = m.Content
			msg["tool_call_id"] = m.ToolCallID
		} else if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			// Assistant message with tool calls
			if m.Content != "" {
				msg["content"] = m.Content
			} else {
				msg["content"] = nil
			}
			var toolCalls []map[string]any
			for _, tc := range m.ToolCalls {
				toolCalls = append(toolCalls, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			msg["tool_calls"] = toolCalls
		} else if m.Role == llm.RoleUser && len(m.Images) > 0 {
			// Multimodal user message: content becomes an array of parts so a
			// vision-capable model receives the images alongside the text. Emitted only
			// when images are attached; the plain string path stays intact so prompt
			// caching of text-only user messages isn't disturbed.
			parts := make([]map[string]any, 0, 1+len(m.Images))
			if m.Content != "" {
				parts = append(parts, map[string]any{
					"type": "text",
					"text": m.Content,
				})
			}
			for _, img := range m.Images {
				if len(img.Data) == 0 {
					continue
				}
				mime := img.MIMEType
				if mime == "" {
					mime = "image/png"
				}
				dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": dataURL,
					},
				})
			}
			msg["content"] = parts
		} else {
			msg["content"] = m.Content
		}

		// Stamp the cache breakpoint on the tail-most Cacheable user messages (no-op
		// unless emitCacheControl is set). Promotes string content to a text part so
		// cache_control has a block to ride on.
		if c.emitCacheControl && cacheableUserIdx[mi] {
			stampOpenAICacheControl(msg, c.cfg.CacheTTL)
		}

		msgs = append(msgs, msg)
	}
	// Stamp the reserved breakpoint on the final message so the whole
	// conversation up to this turn is cached for the next call, mirroring the
	// Anthropic client: with an append-only history a tail breakpoint covers
	// everything before it. Idempotent when the final message already carries
	// one from the Cacheable path.
	if stampFinal && len(msgs) > 0 {
		stampOpenAICacheControl(msgs[len(msgs)-1], c.cfg.CacheTTL)
	}
	return msgs
}

// cacheBreakpoints returns the indices into req.Messages of the user messages that
// should carry a cache_control breakpoint (empty when explicit caching is off), plus
// whether the final message gets the reserved tail breakpoint. Mirrors the Anthropic
// budgeting: at most 4 breakpoints, one for the cached system block, one reserved
// for the final message, the rest on the tail-most Cacheable user messages (a later
// breakpoint's prefix already covers earlier ones). OpenAI-format tools live in a
// separate top-level array, so no breakpoint is reserved for them.
func (c *Client) cacheBreakpoints(req llm.CompletionRequest) (map[int]bool, bool) {
	if !c.emitCacheControl {
		return map[int]bool{}, false
	}
	const maxBreakpoints = 4
	budget := maxBreakpoints
	if req.CacheSystem && req.System != "" {
		budget-- // system block consumes one
	}
	stampFinal := req.CacheSystem && len(req.Messages) > 0
	candidates := req.Messages
	if stampFinal {
		budget-- // final-message block, stamped after the message loop
		candidates = req.Messages[:len(req.Messages)-1]
	}
	return llm.CacheBreakpointIndices(candidates, budget), stampFinal
}

// stampOpenAICacheControl marks an already-built message as a prompt-cache
// breakpoint in the OpenAI content-part format OpenRouter and other
// Anthropic/Gemini-backed gateways accept: a string body is promoted to a text part,
// then cache_control:ephemeral is stamped on the last part so the prefix covers it.
// ttl carries the provider's cache_ttl ("1h" for the extended cache); empty keeps the
// default lifetime, so a gateway that never set cache_ttl is unaffected.
func stampOpenAICacheControl(msg map[string]any, ttl string) {
	switch content := msg["content"].(type) {
	case string:
		msg["content"] = []map[string]any{
			{"type": "text", "text": content, "cache_control": llm.EphemeralCacheControlTTL(ttl)},
		}
	case []map[string]any:
		if len(content) > 0 {
			content[len(content)-1]["cache_control"] = llm.EphemeralCacheControlTTL(ttl)
		}
	}
}

// OpenAI API response types.
type apiResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []apiChoice `json:"choices"`
	Usage   apiUsage    `json:"usage"`
}

type apiChoice struct {
	Index        int        `json:"index"`
	Message      apiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type apiMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
	// ReasoningContent is the Z.ai / DeepSeek-R1 / Qwen-thinking convention for the
	// model's chain-of-thought channel. Tokens here count against completion_tokens
	// but never appear in content or tool_calls, so callers must capture this or
	// discard budget-billed output. Reasoning is the OpenAI / xAI alias; providers
	// populate one or the other, never both.
	ReasoningContent *string       `json:"reasoning_content,omitempty"`
	Reasoning        *string       `json:"reasoning,omitempty"`
	ToolCalls        []apiToolCall `json:"tool_calls,omitempty"`
}

type apiToolCall struct {
	// Index is the per-choice slot a streamed tool_call belongs to. OpenAI's streaming
	// spec lets providers interleave argument deltas for different indices in any
	// order, so only `index` identifies which call a delta belongs to. Pointer so a
	// missing value is distinguishable from index 0 (some gateways omit it on
	// non-streaming responses); the SSE parser falls back to arrival order when nil.
	Index    *int        `json:"index,omitempty"`
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function apiFunction `json:"function"`
}

type apiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// promptTokensDetails carries OpenAI's automatic prompt-cache hit count.
// Populated by GPT-4o family models and o1 models when the prefix exceeds
// the 1024-token automatic-cache threshold.
type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// normalizeStopReason maps an OpenAI finish_reason to our internal stop_reason
// vocabulary (length→max_tokens, stop→end, tool_calls→tool_use), passing any
// other value through unchanged. Shared by the buffered and streaming paths.
func normalizeStopReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "stop":
		return "end"
	case "tool_calls":
		return "tool_use"
	default:
		return finishReason
	}
}

// normalizeUsage converts an OpenAI-shaped usage report into llm.Usage.
//
// OpenAI's prompt_tokens is the TOTAL input and INCLUDES the cached prefix, so the
// cached portion is subtracted out; the engine surfaces the two separately and must
// not double-count. That convention is not universal (Anthropic reports the uncached
// remainder), and this client serves every OpenAI-compatible endpoint: when
// cached > prompt the inclusive reading is disproven, so prompt_tokens is taken at
// face value. InputTokens must never go negative, since it is SUM(input_tokens) in
// the daily budget tally, where a negative row would push a default spend limit away
// from its ceiling and never trip.
func normalizeUsage(promptTokens, completionTokens, cachedTokens int) llm.Usage {
	input := promptTokens - cachedTokens
	if cachedTokens > promptTokens {
		input = promptTokens
	}
	return llm.Usage{
		InputTokens:          max(input, 0),
		OutputTokens:         completionTokens,
		CacheReadInputTokens: cachedTokens,
	}
}

// parseResponse converts an OpenAI API response to our format.
func (c *Client) parseResponse(resp *apiResponse) *llm.CompletionResponse {
	cachedTokens := 0
	if resp.Usage.PromptTokensDetails != nil {
		cachedTokens = resp.Usage.PromptTokensDetails.CachedTokens
	}
	out := &llm.CompletionResponse{
		Model: resp.Model,
		Usage: normalizeUsage(resp.Usage.PromptTokens, resp.Usage.CompletionTokens, cachedTokens),
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		// Normalize OpenAI finish_reason to our internal stop_reason, matching the
		// streaming path. Without it the tool loop's max_tokens continuation never
		// fires for OpenAI-compatible providers.
		out.StopReason = normalizeStopReason(choice.FinishReason)

		if choice.Message.Content != nil {
			out.Content = *choice.Message.Content
		}

		// Capture any reasoning channel the provider populated, preferring
		// reasoning_content over the reasoning alias. Without this, reasoning models
		// that bill the whole output budget into thinking return an empty-looking
		// response.
		if choice.Message.ReasoningContent != nil {
			out.ReasoningContent = *choice.Message.ReasoningContent
		} else if choice.Message.Reasoning != nil {
			out.ReasoningContent = *choice.Message.Reasoning
		}

		for _, tc := range choice.Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	return out
}

// Streaming SSE types for OpenAI.
type streamChunkResponse struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Model   string              `json:"model"`
	Choices []streamChunkChoice `json:"choices"`
	Usage   *apiUsage           `json:"usage,omitempty"`
}

type streamChunkChoice struct {
	Index        int              `json:"index"`
	Delta        streamChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type streamChunkDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          *string       `json:"content,omitempty"`
	ReasoningContent *string       `json:"reasoning_content,omitempty"` // Z.ai / DeepSeek / Qwen-thinking
	Reasoning        *string       `json:"reasoning,omitempty"`         // OpenAI / xAI alias
	ToolCalls        []apiToolCall `json:"tool_calls,omitempty"`
}

// parseSSEStream parses Server-Sent Events from the OpenAI streaming API.
func (c *Client) parseSSEStream(body io.Reader, callback llm.StreamCallback) error {
	// Track tool calls being built across chunks, keyed by the provider-supplied
	// `index` so interleaved argument deltas land in the right slot (the spec allows
	// deltas for different indices in any order). toolOrder preserves emission order
	// so flushed callbacks fire in the order the provider announced each call, not Go
	// map-iteration order.
	toolCalls := make(map[int]*llm.ToolCall)
	toolArgs := make(map[int]*strings.Builder)
	toolOrder := make([]int, 0, 4)
	// fallbackIdx is used only when a provider omits `index` on streamed deltas. It
	// advances on every new ID, assigning indices in arrival order so non-conforming
	// gateways still attach arguments to the right call.
	fallbackIdx := 0
	var usage *llm.Usage
	var stopReason string

	flushToolCalls := func() {
		for _, idx := range toolOrder {
			tc, ok := toolCalls[idx]
			if !ok {
				continue
			}
			if b, ok := toolArgs[idx]; ok {
				tc.Arguments = b.String()
				if tc.Arguments == "" {
					tc.Arguments = "{}"
				}
			}
			callback(llm.StreamChunk{ToolCall: tc})
		}
	}

	err := llm.ScanSSEData(body, "openai", func(data string) error {
		if data == "[DONE]" {
			flushToolCalls()
			callback(llm.StreamChunk{Done: true, StopReason: stopReason, Usage: usage})
			return io.EOF
		}

		var chunk streamChunkResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil
		}

		// Capture usage if present (OpenAI sends it in the final chunks when stream_options is set)
		if chunk.Usage != nil {
			cachedTokens := 0
			if chunk.Usage.PromptTokensDetails != nil {
				cachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			u := normalizeUsage(chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, cachedTokens)
			usage = &u
		}

		if len(chunk.Choices) == 0 {
			return nil
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		// Text content
		if delta.Content != nil && *delta.Content != "" {
			callback(llm.StreamChunk{Delta: *delta.Content})
		}

		// Reasoning-channel tokens (`reasoning_content`, or the `reasoning` alias),
		// surfaced as a separate StreamChunk so accumulators can preserve them without
		// conflating with visible Content. Without this branch the accumulated
		// reasoning never reaches anyone.
		if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
			callback(llm.StreamChunk{Reasoning: *delta.ReasoningContent})
		} else if delta.Reasoning != nil && *delta.Reasoning != "" {
			callback(llm.StreamChunk{Reasoning: *delta.Reasoning})
		}

		// Tool calls (built up incrementally). Each delta carries an `index` for the
		// call it belongs to; argument fragments for different calls can interleave, so
		// route by index, not arrival order. When the provider omits index (some
		// gateways do on a new call's first chunk), advance a counter on each new ID.
		for _, tc := range delta.ToolCalls {
			var idx int
			switch {
			case tc.Index != nil:
				idx = *tc.Index
			case tc.ID != "" || len(toolOrder) == 0:
				// New call announced without an index - assign the next slot.
				idx = fallbackIdx
				fallbackIdx++
			default:
				// Continuation delta with neither index nor ID - append to
				// the most recently announced call.
				idx = toolOrder[len(toolOrder)-1]
			}

			if _, exists := toolCalls[idx]; !exists {
				toolCalls[idx] = &llm.ToolCall{}
				toolArgs[idx] = &strings.Builder{}
				toolOrder = append(toolOrder, idx)
				if tc.Index != nil && *tc.Index >= fallbackIdx {
					fallbackIdx = *tc.Index + 1
				}
			}
			if tc.ID != "" {
				toolCalls[idx].ID = tc.ID
			}
			if tc.Function.Name != "" {
				toolCalls[idx].Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				toolArgs[idx].WriteString(tc.Function.Arguments)
			}
		}

		// Check for finish
		if choice.FinishReason != nil {
			// Map OpenAI finish reasons to our normalized stop_reason.
			stopReason = normalizeStopReason(*choice.FinishReason)
			// Flush tool calls on stop
			if *choice.FinishReason == "tool_calls" {
				flushToolCalls()
				// Clear for next round
				toolCalls = make(map[int]*llm.ToolCall)
				toolArgs = make(map[int]*strings.Builder)
				toolOrder = toolOrder[:0]
				fallbackIdx = 0
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// If we got here without [DONE], send done anyway
	callback(llm.StreamChunk{Done: true, StopReason: stopReason, Usage: usage})
	return nil
}

// Embed generates embedding vectors for the given texts using the OpenAI embeddings
// API (implements llm.EmbeddingProvider). The model id must be supplied by the
// caller; use LLMProvider.EmbeddingModel() to read the per-provider config value.
func (c *Client) Embed(ctx context.Context, texts []string, model string) ([][]float32, error) {
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("openai embed: model is required (set provider config.embedding_model)")
	}

	body := map[string]any{
		"model": model,
		"input": texts,
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", c.embedURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("openai embed: creating request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai embed: API error %d: %s", resp.StatusCode, llm.ReadErrorBody(resp))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("openai embed: decoding response: %w", err)
	}

	embeddings := make([][]float32, len(texts))
	for _, d := range result.Data {
		if d.Index < len(embeddings) {
			embeddings[d.Index] = d.Embedding
		}
	}
	return embeddings, nil
}
