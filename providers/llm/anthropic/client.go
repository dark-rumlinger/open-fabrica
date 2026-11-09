package anthropic

import (
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

const (
	// defaultBaseURL is an API ROOT, not an operation endpoint: every provider
	// base URL in the system names the root and the transport appends the path.
	defaultBaseURL = "https://api.anthropic.com/v1"
	// defaultAuthHeader is Anthropic's credential header. Expressed as the
	// transport's DEFAULT rather than hardcoded at the call site, so a row can
	// still override it through config (a fronting proxy that wants a bearer
	// token, say) without needing a transport of its own.
	defaultAuthHeader = "x-api-key"
	// anthropicVersionHeader pins the Messages API version. A static per-vendor
	// header, so it lives in the same place OpenRouter's attribution pair does:
	// config.extra_headers, defaulted here.
	anthropicVersionHeader  = "anthropic-version"
	defaultAnthropicVersion = "2023-06-01"

	transportID = "anthropic"
)

// Client implements llm.Provider for the Anthropic API - the one wire protocol
// in the tree that is genuinely not OpenAI-shaped. It keeps a separate system
// field, uses input_schema for tools, and implements structured output as a
// forced single tool rather than a response format, so it cannot be expressed
// as an openai-chat row no matter how much config you give it.
type Client struct {
	name       string
	apiKey     string
	endpoint   string // messages
	modelsURL  string // catalog listing
	httpClient *http.Client
	cfg        providercfg.Config
}

func init() {
	llm.RegisterProvider(transportID, func(o llm.Options) llm.Provider {
		return New(o)
	})
}

// New creates a new Anthropic provider. Options.BaseURL is optional (empty uses
// api.anthropic.com) and is an API root; Options.HTTPClient is optional (nil
// installs a 6-minute timeout client).
func New(o llm.Options) *Client {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: llm.DefaultHTTPTimeout}
	}
	modelsURL := o.ModelsURL
	if modelsURL == "" {
		modelsURL = o.Endpoint(defaultBaseURL, "models")
	}
	return &Client{
		name:       o.Name,
		apiKey:     o.APIKey,
		endpoint:   o.Endpoint(defaultBaseURL, "messages"),
		modelsURL:  modelsURL,
		httpClient: hc,
		cfg:        o.Config.WithDefaults(defaultAuth, defaultHeaders),
	}
}

// defaultAuth and defaultHeaders are Anthropic's own requirements, layered
// under whatever the row states so a plain seed entry needs no config at all
// while an operator fronting Anthropic with a proxy can still override them.
var (
	defaultAuth    = providercfg.Auth{Scheme: providercfg.SchemeHeader, Header: defaultAuthHeader}
	defaultHeaders = map[string]string{anthropicVersionHeader: defaultAnthropicVersion}
)

func (c *Client) Name() string { return c.name }
func (c *Client) Type() string { return transportID }

// SupportsStructuredOutput reports that Anthropic can emit a strict-shape
// response by forcing a single tool whose input_schema is the requested
// schema; the engine extracts the tool input and surfaces it as content.
func (c *Client) SupportsStructuredOutput() bool { return true }

// ListModels fetches Anthropic's catalog from /v1/models. Each model object carries
// max_tokens (the output cap) and a capabilities tree, read per model instead of
// guessing from the name. Fields the API omits (an older version, or a compatible
// proxy) fall back to conservative defaults: the max_tokens heuristic, and
// streaming/tools/vision = true (true of every Claude model).
//
// The endpoint pages at 20 by default, so the request asks for the maximum page
// size (1000) and follows has_more/last_id until the list is exhausted.
func (c *Client) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	setHeaders := func(r *http.Request) {
		httpauth.Apply(r, c.cfg.ListingAuth(), c.apiKey)
		httpauth.SetExtra(r, c.cfg.ExtraHeaders)
	}
	var out []llm.ModelInfo
	pageURL := llm.WithQuery(c.modelsURL, "limit", "1000")
	for page := 0; page < llm.MaxListPages; page++ {
		payload, err := llm.FetchJSON[modelsPage](ctx, c.httpClient, pageURL, "anthropic list models", setHeaders)
		if err != nil {
			return nil, err
		}
		for _, m := range payload.Data {
			out = append(out, m.info())
		}
		if !payload.HasMore || payload.LastID == "" {
			break
		}
		pageURL = llm.WithQuery(llm.WithQuery(c.modelsURL, "limit", "1000"), "after_id", payload.LastID)
	}
	return out, nil
}

// modelsPage is one page of GET /v1/models.
type modelsPage struct {
	Data    []modelEntry `json:"data"`
	HasMore bool         `json:"has_more"`
	LastID  string       `json:"last_id"`
}

type modelEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	// max_tokens is the per-response OUTPUT cap; max_input_tokens is the
	// context window. They feed the two separate ModelInfo fields that replaced
	// a single conflated one. Zero when absent.
	MaxTokens      int `json:"max_tokens"`
	MaxInputTokens int `json:"max_input_tokens"`
	// capabilities.<name>.supported. Pointers distinguish "absent" (older API,
	// keep the default) from "present and false" (a real signal).
	Capabilities struct {
		ImageInput *struct {
			Supported bool `json:"supported"`
		} `json:"image_input"`
		Thinking *struct {
			Supported bool `json:"supported"`
		} `json:"thinking"`
	} `json:"capabilities"`
}

// info maps one listing entry onto ModelInfo.
func (m modelEntry) info() llm.ModelInfo {
	display := m.DisplayName
	if display == "" {
		display = m.ID
	}
	// The Models API reports max_tokens (the OUTPUT cap) and max_input_tokens
	// (the window). They land in different ModelInfo fields; the window stays 0
	// when the API omits it.
	outputCap := m.MaxTokens
	if outputCap <= 0 {
		outputCap = fallbackMaxOutputTokens // API didn't report it
	}
	// Vision: trust the flag when present; default true when the API omits the
	// capabilities tree (every current Claude model is multimodal).
	vision := true
	if m.Capabilities.ImageInput != nil {
		vision = m.Capabilities.ImageInput.Supported
	}
	return llm.ModelInfo{
		ID:              m.ID,
		DisplayName:     display,
		ContextTokens:   m.MaxInputTokens,
		MaxOutputTokens: outputCap,
		// Streaming and tool use have no capabilities flag and are true of
		// every Claude model, so they stay true.
		SupportsStreaming: true,
		SupportsTools:     true,
		SupportsVision:    vision,
		SupportsReasoning: m.Capabilities.Thinking != nil && m.Capabilities.Thinking.Supported,
	}
}

// fallbackMaxOutputTokens is the model-agnostic output cap used ONLY when the Models
// API doesn't report max_tokens (an older API version or a compatible proxy); real
// api.anthropic.com carries its own value per model. Deliberately name-free: 64K is
// the smallest output cap across the current Claude line, so it never over-promises,
// and it is a display/config default (the engine sets its own per-stage max_tokens).
// The admin UI can raise it per model if needed.
const fallbackMaxOutputTokens = 64000

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

// Complete sends a non-streaming completion request. A rejected sampling param
// or effort control is learned once per model and the request retried without it
// (see providers/llm/model_caps_learn.go).
func (c *Client) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return llm.RetryOnQuirk(transportID, c.capKey(req.Model), req, func() (*llm.CompletionResponse, error) {
		return c.completeOnce(ctx, req)
	})
}

// capKey returns the capability-store key for this client and model.
func (c *Client) capKey(model string) llm.ModelKey {
	return llm.ModelKey{Provider: c.name, Model: model}
}

func (c *Client) completeOnce(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	var apiResp apiResponse
	if err := llm.PostJSON(ctx, c.httpClient, c.endpoint, c.name, c.buildRequestBody(req, false), c.setHeaders, &apiResp); err != nil {
		return nil, err
	}
	return c.parseResponse(&apiResp, req.ResponseSchema != nil), nil
}

// Stream sends a streaming completion request and calls the callback for each
// chunk. The learn-and-retry is safe here: a sampling/effort rejection is an
// HTTP 400 classified before any SSE bytes reach the callback.
func (c *Client) Stream(ctx context.Context, req llm.CompletionRequest, callback llm.StreamCallback) error {
	return llm.RetryOnQuirkErr(transportID, c.capKey(req.Model), req, func() error {
		return c.streamOnce(ctx, req, callback)
	})
}

func (c *Client) streamOnce(ctx context.Context, req llm.CompletionRequest, callback llm.StreamCallback) error {
	resp, err := llm.PostJSONStream(ctx, c.httpClient, c.endpoint, c.name, c.buildRequestBody(req, true), c.setHeaders)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return c.parseSSEStream(resp.Body, callback)
}

// setHeaders sets common headers for Anthropic API requests. The credential
// placement and the version pin come from config (defaulted in New), not from
// literals here, so a compatible proxy in front of Anthropic is a row rather
// than a code change.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	httpauth.Apply(req, c.cfg.Auth, c.apiKey)
	httpauth.SetExtra(req, c.cfg.ExtraHeaders)
}

// sanitizeToolInputSchema adapts a generic JSON Schema for use as an Anthropic
// forced-tool `input_schema`, which rejects oneOf/allOf/anyOf AT THE TOP LEVEL though
// it accepts them nested inside properties. The engine's PLAN schema carries a
// top-level anyOf as a grammar-level nudge; the real requirement is re-checked after
// decode (the empty-queue corrective retry in stage_plan.go), so dropping the
// top-level combinator loses no enforcement. Only the root keys are removed; nested
// combinators are left intact. Non-map schemas pass through unchanged.
func sanitizeToolInputSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	for _, key := range []string{"oneOf", "allOf", "anyOf"} {
		delete(m, key)
	}
	return m
}

// buildRequestBody converts our generic request into the Anthropic API format.
func (c *Client) buildRequestBody(req llm.CompletionRequest, stream bool) map[string]any {
	body := map[string]any{
		"model": req.Model,
	}
	// Every capability read below goes through this key.
	k := c.capKey(req.Model)
	// Learned-only: a declared supports_vision=0 usually means the discovery
	// heuristic did not recognise the model name, not that it cannot see.
	if llm.Caps.Learned(k, llm.CapVision) == llm.No {
		req, _ = llm.StripImages(req)
	}

	// Anthropic uses a separate "system" field, not a system message in the array.
	// When caching is enabled, format as content blocks with cache_control.
	if req.System != "" {
		if req.CacheSystem {
			body["system"] = []map[string]any{
				{
					"type":          "text",
					"text":          req.System,
					"cache_control": llm.EphemeralCacheControlTTL(c.cfg.CacheTTL),
				},
			}
		} else {
			body["system"] = req.System
		}
	}

	// Anthropic allows at most 4 cache_control breakpoints per request. The cached
	// system block and the last-tool block each consume one when CacheSystem is set,
	// and one more is reserved for the FINAL message: with an append-only history a
	// breakpoint on the tail covers the whole conversation, so the next call reads
	// everything up to here from cache. The Cacheable user messages share what's left,
	// stamped on only the LAST few (a later breakpoint's prefix already covers earlier
	// ones). Exceeding 4 is a hard API error, so this is a correctness guard.
	const maxCacheBreakpoints = 4
	baseBreakpoints := 0
	if req.CacheSystem && req.System != "" {
		baseBreakpoints++ // system block
	}
	if req.CacheSystem && req.ResponseSchema == nil && len(req.Tools) > 0 {
		baseBreakpoints++ // last-tool block
	}
	stampFinalMessage := req.CacheSystem && len(req.Messages) > 0
	if stampFinalMessage {
		baseBreakpoints++ // final-message block (stamped after the message loop)
	}
	userCacheBudget := maxCacheBreakpoints - baseBreakpoints
	// Indices (into req.Messages) of the Cacheable user messages we will stamp: the
	// last userCacheBudget of them. The final message is excluded (via the prefix
	// slice); the reserved tail breakpoint covers it, so counting it here would
	// waste a slot.
	candidates := req.Messages
	if stampFinalMessage {
		candidates = req.Messages[:len(req.Messages)-1]
	}
	cacheableUserIdx := llm.CacheBreakpointIndices(candidates, userCacheBudget)

	// Convert messages (skip system role since we handle it above)
	var msgs []map[string]any
	for mi, m := range req.Messages {
		if m.Role == llm.RoleSystem {
			// If system wasn't set in the request but there's a system message, use it
			if req.System == "" {
				body["system"] = m.Content
			}
			continue
		}
		// An assistant turn with no text and no tool calls (an empty or cut-off
		// reply kept in the transcript) has no wire form: Anthropic rejects empty
		// content on any message but a final prefill. Leaving it out lets the
		// neighbouring user turns merge.
		if m.Role == llm.RoleAssistant && m.Content == "" && len(m.ToolCalls) == 0 {
			continue
		}

		msg := convertAnthropicMessage(m, cacheableUserIdx[mi], c.cfg.CacheTTL)

		msgs = append(msgs, msg)
	}
	// Stamp the reserved breakpoint on the final message so the whole conversation up
	// to this turn is cached for the next call. Works on any role's content shape; a
	// string content is wrapped into a block first. Idempotent when the final message
	// already carries a breakpoint from the Cacheable path.
	if stampFinalMessage && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if s, ok := last["content"].(string); ok && s != "" {
			last["content"] = []map[string]any{
				{"type": "text", "text": s},
			}
		}
		if blocks, ok := last["content"].([]map[string]any); ok && len(blocks) > 0 {
			blocks[len(blocks)-1]["cache_control"] = llm.EphemeralCacheControlTTL(c.cfg.CacheTTL)
			last["content"] = blocks
		}
	}
	body["messages"] = msgs

	// Max tokens (required by Anthropic)
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body["max_tokens"] = maxTokens

	// Sampling params are omitted for models that reject them: prompting is
	// Anthropic's steering surface there.
	if llm.Caps.Allowed(k, llm.CapSamplingParams) {
		if req.Temperature != nil {
			body["temperature"] = *req.Temperature
		}
		if req.TopP != nil {
			body["top_p"] = *req.TopP
		}
	}

	// Effort (how hard the model deliberates per turn) goes under output_config,
	// GA - no beta header. Sent only for models known to accept it; the newer
	// models otherwise default to "high", their slowest.
	if req.Effort != "" && llm.Caps.Allowed(k, llm.CapEffort) {
		body["output_config"] = map[string]any{"effort": req.Effort}
	}
	if len(req.StopWords) > 0 {
		body["stop_sequences"] = req.StopWords
	}

	// Structured output (ResponseSchema) is implemented by injecting a single forced
	// tool whose input_schema is the schema. This takes precedence over any
	// caller-supplied tools because tool_choice=tool would override them anyway. The
	// engine extracts the tool input and exposes it as Content.
	if req.ResponseSchema != nil {
		var schema any
		if err := json.Unmarshal(req.ResponseSchema.Schema, &schema); err != nil {
			schema = map[string]any{}
		}
		schema = sanitizeToolInputSchema(schema)
		toolName := req.ResponseSchema.Name
		if toolName == "" {
			toolName = "emit_response"
		}
		body["tools"] = []map[string]any{{
			"name":         toolName,
			"description":  "Return the response as a single call to this tool.",
			"input_schema": schema,
		}}
		// Force the single tool where the model accepts it; parseResponse surfaces the
		// tool input as Content. The newer models (Sonnet 5.5, Opus 5.5, Fable 5.1) 400
		// on a forced tool_choice, so there the model is steered to the one tool by its
		// description under tool_choice=auto instead, and the 400 is learned once (see
		// quirks.go). Native output_config.format is deliberately NOT the fallback: it
		// demands a strict schema subset (additionalProperties:false, no numeric/length
		// bounds, limited patterns) the engine's lenient response schemas do not meet.
		if llm.Caps.Allowed(k, llm.CapForcedToolChoice) {
			body["tool_choice"] = map[string]any{"type": "tool", "name": toolName}
		} else {
			body["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": true}
		}
	} else if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			if t.Type != "" {
				// Server-side tool (e.g., web_search).
				apiType := t.Type
				if apiType == "web_search" {
					apiType = "web_search_20250305" // Anthropic-specific version
				}
				tools = append(tools, map[string]any{
					"type": apiType,
					"name": t.Name,
				})
			} else {
				tool := map[string]any{
					"name":         t.Name,
					"description":  t.Description,
					"input_schema": t.Parameters,
				}
				tools = append(tools, tool)
			}
		}
		// Add cache_control to the last tool so the entire tool set is cached.
		if req.CacheSystem && len(tools) > 0 {
			tools[len(tools)-1]["cache_control"] = llm.EphemeralCacheControlTTL(c.cfg.CacheTTL)
		}
		body["tools"] = tools
	}

	if stream {
		body["stream"] = true
	}

	return body
}

// apiResponse represents the Anthropic messages API response.
type apiResponse struct {
	ID         string       `json:"id"`
	Type       string       `json:"type"`
	Role       string       `json:"role"`
	Content    []apiContent `json:"content"`
	Model      string       `json:"model"`
	StopReason string       `json:"stop_reason"`
	Usage      apiUsage     `json:"usage"`
}

type apiContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type apiUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// parseResponse converts an Anthropic API response to our format. When
// structuredOutput is true, the response is a forced tool_use block; its input is
// surfaced as Content so the engine decodes it like any other JSON payload.
func (c *Client) parseResponse(resp *apiResponse, structuredOutput bool) *llm.CompletionResponse {
	out := &llm.CompletionResponse{
		Model:      resp.Model,
		StopReason: resp.StopReason,
		Usage: llm.Usage{
			InputTokens:              resp.Usage.InputTokens,
			OutputTokens:             resp.Usage.OutputTokens,
			CacheReadInputTokens:     resp.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: resp.Usage.CacheCreationInputTokens,
		},
	}

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			out.Content += block.Text
		case "tool_use":
			if structuredOutput {
				out.Content = string(block.Input)
				continue
			}
			out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
			})
		case "server_tool_use", "web_search_tool_result":
			// Server-side tool results, handled internally by Claude.
		}
	}

	return out
}

// parseSSEStream parses Server-Sent Events from the Anthropic streaming API.
func (c *Client) parseSSEStream(body io.Reader, callback llm.StreamCallback) error {
	// Track tool calls being built across chunks
	var currentToolCall *llm.ToolCall
	var toolArgsBuilder strings.Builder
	var usage *llm.Usage
	var stopReason string

	err := llm.ScanSSEData(body, c.name, func(data string) error {
		if data == "[DONE]" {
			callback(llm.StreamChunk{Done: true, StopReason: stopReason, Usage: usage})
			return io.EOF
		}

		var event sseEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil
		}

		switch event.Type {
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				currentToolCall = &llm.ToolCall{
					ID:   event.ContentBlock.ID,
					Name: event.ContentBlock.Name,
				}
				toolArgsBuilder.Reset()
			}

		case "content_block_delta":
			if event.Delta != nil {
				switch event.Delta.Type {
				case "text_delta":
					callback(llm.StreamChunk{Delta: event.Delta.Text})
				case "input_json_delta":
					toolArgsBuilder.WriteString(event.Delta.PartialJSON)
				}
			}

		case "content_block_stop":
			if currentToolCall != nil {
				currentToolCall.Arguments = toolArgsBuilder.String()
				if currentToolCall.Arguments == "" {
					currentToolCall.Arguments = "{}"
				}
				callback(llm.StreamChunk{ToolCall: currentToolCall})
				currentToolCall = nil
				toolArgsBuilder.Reset()
			}

		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
			}
			// message_delta carries the final cumulative output_tokens; the input and
			// cache counts arrive once in message_start and must NOT be clobbered here
			// (the delta usage omits them, so a naive replace would zero them).
			if event.Usage != nil {
				if usage == nil {
					usage = &llm.Usage{}
				}
				usage.OutputTokens = event.Usage.OutputTokens
				// Adopt input/cache only when the delta actually carries them, so
				// we never overwrite the message_start values with a stray zero.
				if event.Usage.InputTokens > 0 {
					usage.InputTokens = event.Usage.InputTokens
				}
				if event.Usage.CacheReadInputTokens > 0 {
					usage.CacheReadInputTokens = event.Usage.CacheReadInputTokens
				}
				if event.Usage.CacheCreationInputTokens > 0 {
					usage.CacheCreationInputTokens = event.Usage.CacheCreationInputTokens
				}
			}

		case "message_start":
			// message_start carries the full input accounting, including the cache_read /
			// cache_creation split. Capture all four fields and don't gate on
			// input_tokens > 0 (a fully cache-read prompt reports input_tokens = 0 with a
			// large cache_read_input_tokens).
			if event.Message != nil {
				u := event.Message.Usage
				usage = &llm.Usage{
					InputTokens:              u.InputTokens,
					OutputTokens:             u.OutputTokens,
					CacheReadInputTokens:     u.CacheReadInputTokens,
					CacheCreationInputTokens: u.CacheCreationInputTokens,
				}
			}

		case "message_stop":
			callback(llm.StreamChunk{Done: true, StopReason: stopReason, Usage: usage})
			return io.EOF

		case "error":
			errMsg := "unknown error"
			if event.Error != nil {
				errMsg = event.Error.Message
			}
			streamErr := fmt.Errorf("anthropic: stream error: %s", errMsg)
			callback(llm.StreamChunk{Error: streamErr})
			return streamErr
		}
		return nil
	})
	if err != nil {
		return err
	}

	// If we got here without a message_stop, send done anyway
	callback(llm.StreamChunk{Done: true, StopReason: stopReason, Usage: usage})
	return nil
}

// SSE event types for Anthropic streaming.
type sseEvent struct {
	Type         string       `json:"type"`
	Delta        *sseDelta    `json:"delta,omitempty"`
	ContentBlock *sseBlock    `json:"content_block,omitempty"`
	Message      *apiResponse `json:"message,omitempty"`
	Usage        *apiUsage    `json:"usage,omitempty"`
	Error        *apiError    `json:"error,omitempty"`
	Index        int          `json:"index,omitempty"`
}

type sseDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type sseBlock struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
}

// convertAnthropicMessage converts one llm.Message into the Anthropic wire shape
// (tool_result / tool_use / multimodal image / plain text). stampCache requests an
// ephemeral cache_control breakpoint on the final content block of a Cacheable user
// message; the caller decides which messages fit the 4-breakpoint budget. cacheTTL
// carries the configured entry lifetime (empty means the 5m default).
func convertAnthropicMessage(m llm.Message, stampCache bool, cacheTTL string) map[string]any {
	msg := map[string]any{
		"role": string(m.Role),
	}

	if m.Role == llm.RoleTool {
		// Tool results in Anthropic format
		msg["role"] = "user"
		msg["content"] = []map[string]any{
			{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			},
		}
	} else if len(m.ToolCalls) > 0 {
		// Assistant message with tool calls
		content := []map[string]any{}
		if m.Content != "" {
			content = append(content, map[string]any{
				"type": "text",
				"text": m.Content,
			})
		}
		for _, tc := range m.ToolCalls {
			var args any
			if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
				args = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Name,
				"input": args,
			})
		}
		msg["content"] = content
	} else if m.Role == llm.RoleUser && len(m.Images) > 0 {
		// Multimodal user message: text + base64 image blocks. Anthropic takes image
		// data directly (no data-URL prefix) in a structured source object. Emitted
		// only when images are present so text-only user messages keep string content
		// and the prompt cache stays hot.
		parts := []map[string]any{}
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
			parts = append(parts, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mime,
					"data":       base64.StdEncoding.EncodeToString(img.Data),
				},
			})
		}
		msg["content"] = parts
	} else {
		msg["content"] = m.Content
	}

	// Cacheable user messages become a prompt-cache breakpoint: wrap string content as
	// a content-array, then stamp cache_control:ephemeral on the last block (Anthropic
	// only honours it on blocks, not bare strings). Non-user roles are skipped. Gated
	// by cacheableUserIdx so the total never exceeds the 4-breakpoint cap.
	if m.Cacheable && m.Role == llm.RoleUser && stampCache {
		if s, ok := msg["content"].(string); ok {
			msg["content"] = []map[string]any{
				{"type": "text", "text": s},
			}
		}
		if blocks, ok := msg["content"].([]map[string]any); ok && len(blocks) > 0 {
			blocks[len(blocks)-1]["cache_control"] = llm.EphemeralCacheControlTTL(cacheTTL)
			msg["content"] = blocks
		}
	}
	return msg
}
