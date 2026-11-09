// Package ollama exposes a local Ollama daemon as a first-class provider.
// Ollama's chat endpoint is OpenAI-compatible (drop-in via openai.Client),
// but two things sit outside that surface and need a wrapper:
//  1. Catalog discovery lives at GET /api/tags, a different path the
//     generic OpenAI lister cannot derive.
//  2. Structured outputs. The OpenAI-compat layer does not honour
//     response_format=json_schema reliably; the native /api/chat path
//     with a top-level "format" field is the documented surface, so
//     Complete switches to it whenever a ResponseSchema is set.
package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"openfabrica/internal/jsonrepair"
	"openfabrica/providers/internal/httpauth"
	"openfabrica/providers/llm"
	"openfabrica/providers/llm/openai"
)

// defaultBaseURL is an API ROOT. The native /api/chat path is a SIBLING of the
// /v1 surface, not a child of it, which is why it is derived with
// Options.Sibling rather than by trimming a suffix off the chat endpoint.
const defaultBaseURL = "http://localhost:11434/v1"

const transportID = "ollama"

func init() {
	llm.RegisterProvider(transportID, func(o llm.Options) llm.Provider {
		return New(o)
	})
}

// Client embeds an OpenAI-compatible client for the free-text completion
// path and overlays Ollama-specific Type(), catalog discovery, and a
// native /api/chat path for structured outputs.
type Client struct {
	*openai.Client

	// nativeURL is <root>/api/chat, resolved at construction. Ollama's native
	// endpoint sits beside the OpenAI-compatible /v1 surface, so it is derived
	// from the base root rather than recovered from the chat endpoint.
	nativeURL string
	// keepAlive is sent as the native request's "keep_alive". A build runs many
	// same-prefix turns seconds apart; keeping the model (and its slot KV cache)
	// resident lets the daemon reuse the cached prefix instead of re-evaluating
	// the full ~15k-token prompt every turn. Empty omits the field.
	keepAlive string
	// numCtx sets native options.num_ctx. The stable build prefix only stays
	// cache-eligible if it fits the context window; a window smaller than the
	// prompt forces a context shift that evicts the cached prefix. 0 omits the
	// field and uses the model's own default window.
	numCtx int
}

func New(o llm.Options) *Client {
	return &Client{
		Client:    openai.New(o),
		nativeURL: o.Sibling(defaultBaseURL, "api", "chat"),
		keepAlive: ollamaKeepAlive(),
		numCtx:    ollamaNumCtx(),
	}
}

// Type reports "ollama" rather than the embedded client's "openai-chat". Go
// embedding has no virtual dispatch, so this is only visible to callers holding
// the *Client (or the llm.Provider interface) - code inside openai.Client still
// labels its own errors openai-chat, which is correct: that is the protocol the
// request actually spoke.
func (c *Client) Type() string { return transportID }

// ollamaKeepAlive resolves the keep_alive value sent on native requests.
// Defaults to "30m" so the model and its prefix KV cache survive a build's
// turn-to-turn gaps; OPENFABRICA_OLLAMA_KEEP_ALIVE overrides (e.g. "-1" to keep
// the model resident indefinitely, "0" to unload immediately after each call).
func ollamaKeepAlive() string {
	if v := strings.TrimSpace(os.Getenv("OPENFABRICA_OLLAMA_KEEP_ALIVE")); v != "" {
		return v
	}
	return "30m"
}

// keepAliveValue renders the resolved keep_alive setting for the native request.
// Ollama accepts keep_alive as either a duration STRING ("30m", "1h") or a raw
// number of seconds, where -1 means "resident indefinitely" and 0 "unload after
// each call". A bare integer like "-1" is NOT a valid Go duration (Ollama parses a
// string keep_alive with time.ParseDuration, which rejects a unit-less value), so
// sending it as a JSON string makes the daemon reject it and silently fall back to
// its default - defeating OPENFABRICA_OLLAMA_KEEP_ALIVE=-1, which the README tells
// users to set. So an all-integer value (-1/0/seconds) is emitted as a JSON
// number; a real duration like "30m" stays a string.
func keepAliveValue(s string) any {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return s
}

// ollamaNumCtx resolves native options.num_ctx. 0 (the default) omits it and
// uses the model's own context window. Set OPENFABRICA_OLLAMA_NUM_CTX to a value
// at least as large as the build prefix (~16k+ tokens) so the cached prefix is
// not evicted by a context shift on a model whose default window is smaller.
func ollamaNumCtx() int {
	if v := strings.TrimSpace(os.Getenv("OPENFABRICA_OLLAMA_NUM_CTX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ContextWindow reports the window the operator configured with
// OPENFABRICA_OLLAMA_NUM_CTX, or 0 when unset (the model's own default is then in
// force and not known here). The engine sizes its context rollover from it.
func (c *Client) ContextWindow() int { return ollamaNumCtx() }

// SupportsStructuredOutput reports true: Complete below routes to the
// native /api/chat endpoint when ResponseSchema is set, which Ollama
// honours by token-constrained generation against the supplied JSON
// schema. Whether a specific local model can satisfy a non-trivial
// schema is model-dependent, so the native path verifies the reply is
// real JSON and surfaces CodeSchemaDrift when the daemon answered 200
// without constraining the output.
func (c *Client) SupportsStructuredOutput() bool { return true }

// Complete delegates to the OpenAI-compatible base unless the request needs an
// Ollama-specific control the compat layer silently ignores (enable_thinking/
// thinking.type are vLLM/OpenRouter conventions, not Ollama's):
//   - ResponseSchema -> the native "format" field (token-constrained output)
//   - Thinking        -> the native "think" field (reasoning on/off)
//
// Such requests route to the native /api/chat endpoint, which also forwards tools
// and parses tool calls, so a thinking-controlled tool loop (BUILD/LIVE set
// Thinking=false to stop a reasoning model from burning its whole output budget on
// a hidden chain-of-thought) routes here too - otherwise the reasoning toggle is
// dropped.
func (c *Client) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	// Route to native whenever the request needs an Ollama-specific control the
	// compat layer drops: a response schema, the reasoning toggle, or a configured
	// num_ctx. num_ctx and keep_alive ONLY ride the native path; a request that
	// falls through to compat loses them. That matters because num_ctx sizes the
	// context window to hold the stable build prefix - drop it and a context shift
	// evicts the daemon's cached prefix (the README warns about exactly this). So
	// when an admin has set OPENFABRICA_OLLAMA_NUM_CTX (numCtx > 0) every call goes
	// native, never silently dropping it. keep_alive rides native too; engine stages
	// always set Thinking (engine/spec.go), so build/live turns - the only ones that
	// benefit from a resident model - already take this path. A plain completion with
	// no schema, no Thinking, and no num_ctx stays on the compat path unchanged.
	needsNative := req.ResponseSchema != nil || req.Thinking != nil || c.numCtx > 0
	if !needsNative {
		return c.Client.Complete(ctx, req)
	}
	return c.completeNative(ctx, req)
}

// completeNative posts to <root>/api/chat, carrying the JSON schema in "format"
// (when set), the reasoning toggle in "think", and any tools in "tools". It
// forwards the full message history including assistant tool_calls and tool
// results, and parses tool calls back out of the reply, so a tool-driven loop
// (BUILD/LIVE) runs here with the reasoning toggle honoured.
func (c *Client) completeNative(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	chatURL := c.nativeURL
	if chatURL == "" {
		return nil, &llm.Error{
			Code:    llm.CodeSchemaUnsupported,
			Message: "ollama: cannot derive /api/chat URL from base",
		}
	}

	body := map[string]any{
		"model":    req.Model,
		"messages": nativeMessages(req),
		"stream":   false,
	}
	if c.keepAlive != "" {
		body["keep_alive"] = keepAliveValue(c.keepAlive)
	}
	if tools := nativeTools(req.Tools); len(tools) > 0 {
		body["tools"] = tools
	}

	schemaSet := req.ResponseSchema != nil
	if schemaSet {
		var schema any
		if err := json.Unmarshal(req.ResponseSchema.Schema, &schema); err != nil {
			return nil, &llm.Error{
				Code:    llm.CodeBadRequest,
				Message: fmt.Sprintf("ollama: response schema is not valid JSON: %v", err),
				Cause:   err,
			}
		}
		body["format"] = schema
	}

	// Reasoning toggle. An explicit caller value wins. With none set, a
	// structured call still defaults thinking OFF: chain-of-thought and a
	// from-token-zero JSON grammar conflict, and a reasoning model forced to
	// emit the document before its <think> phase destabilises into malformed
	// output the grammar fails to catch.
	// Omitted entirely for daemons or models that have rejected the field before.
	think := false
	if req.Thinking != nil {
		think = *req.Thinking
	}
	if llm.Caps.Allowed(c.CapKey(req.Model), llm.CapNativeThinkField) {
		body["think"] = think
	}

	options := map[string]any{}
	if req.MaxTokens > 0 {
		// Ollama exposes the response cap as num_predict in its options
		// block; max_tokens is silently ignored here.
		options["num_predict"] = req.MaxTokens
	}
	if c.numCtx > 0 {
		// Size the context window to hold the stable build prefix so the
		// daemon's cached prefix is not evicted by a context shift.
		options["num_ctx"] = c.numCtx
	}
	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		options["top_p"] = *req.TopP
	}
	if len(req.StopWords) > 0 {
		options["stop"] = req.StopWords
	}
	if schemaSet {
		// A schema-constrained call (PLAN) emits one large structured document.
		// A weaker model can fall into a degenerate loop, repeating a phrase
		// inside a string value until it exhausts num_predict. A mild repeat
		// penalty over a wide window breaks that loop. It is scoped to the
		// structured path so it never reweights a tool-loop's code output,
		// where legitimate token repetition is normal. The grammar already
		// forces structural tokens, so the penalty only bites free-text values.
		options["repeat_penalty"] = 1.2
		options["repeat_last_n"] = 256
	}
	if len(options) > 0 {
		body["options"] = options
	}

	// Older daemons and non-thinking models reject "think". We only send it to
	// suppress reasoning, so dropping it and retrying keeps those setups working.
	return llm.RetryOnQuirk(transportID, c.CapKey(req.Model), req, func() (*llm.CompletionResponse, error) {
		if !llm.Caps.Allowed(c.CapKey(req.Model), llm.CapNativeThinkField) {
			delete(body, "think")
		}
		return c.postNativeChat(ctx, chatURL, body, schemaSet)
	})
}

// nativeMessages renders req into Ollama's /api/chat message array, preserving
// assistant tool_calls and tool-result messages so a tool-driven loop survives
// the native path. Ollama carries tool-call arguments as a JSON object, not the
// JSON string llm.ToolCall uses, so each is decoded here (and re-encoded on the
// way back in postNativeChat).
func nativeMessages(req llm.CompletionRequest) []map[string]any {
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		switch {
		case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
			tcs := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": argsToObject(tc.Arguments),
					},
				})
			}
			msgs = append(msgs, map[string]any{
				"role":       "assistant",
				"content":    m.Content,
				"tool_calls": tcs,
			})
		case m.Role == llm.RoleTool:
			msgs = append(msgs, map[string]any{"role": "tool", "content": m.Content})
		default:
			msgs = append(msgs, map[string]any{"role": string(m.Role), "content": m.Content})
		}
	}
	return msgs
}

// argsToObject decodes a tool-call arguments JSON string into the object Ollama's
// native API expects. An empty or unparseable value falls back to an empty object
// so a malformed prior call can't abort the whole request.
func argsToObject(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return map[string]any{}
	}
	return v
}

// nativeTools renders the tool defs into Ollama's /api/chat "tools" array (the
// same function-tool shape OpenAI uses). Returns nil when no tools are set.
func nativeTools(defs []llm.ToolDef) []map[string]any {
	if len(defs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(defs))
	for _, t := range defs {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return out
}

// postNativeChat marshals body, posts it to /api/chat, and decodes the reply.
// When verifyJSON is set (a schema'd call), it also confirms the model honoured
// the structured-output schema: Ollama answers 200 even when it did NOT
// constrain decoding (a reasoning model fighting the grammar, or a server-side
// schema->grammar build that silently fell back to free generation), so
// unconstrained, malformed text would otherwise reach the planner as a
// "successful" response and get re-prompted identically. A non-JSON body is
// surfaced as typed CodeSchemaDrift (or CodeTruncated when the cap was hit) so
// the retry/fallback layer can route around it. Free-text calls (e.g. TITLE)
// pass verifyJSON=false, since their output is not expected to be JSON.
func (c *Client) postNativeChat(ctx context.Context, chatURL string, body map[string]any, verifyJSON bool) (*llm.CompletionResponse, error) {
	var apiResp struct {
		Model   string `json:"model"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// Thinking carries a reasoning channel some models/templates emit
			// separately from content. We decode it so a turn whose whole output
			// landed here is not mistaken for an empty response, and so a tool
			// call a model wrote into its reasoning instead of tool_calls is
			// still recoverable for diagnosis.
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments any    `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		Done            bool   `json:"done"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
	}
	setHeaders := func(r *http.Request) { httpauth.SetBearer(r, c.APIKey()) }
	if err := llm.PostJSON(ctx, c.HTTPClient(), chatURL, "ollama", body, setHeaders, &apiResp); err != nil {
		return nil, err
	}

	// Ollama returns tool-call arguments as a JSON object; llm.ToolCall carries
	// them as a JSON string, so re-encode each. A string-typed arguments (some
	// daemons) passes through unchanged.
	var toolCalls []llm.ToolCall
	for _, tc := range apiResp.Message.ToolCalls {
		args := "{}"
		switch a := tc.Function.Arguments.(type) {
		case string:
			if strings.TrimSpace(a) != "" {
				args = a
			}
		case nil:
		default:
			if b, mErr := json.Marshal(a); mErr == nil {
				args = string(b)
			}
		}
		toolCalls = append(toolCalls, llm.ToolCall{Name: tc.Function.Name, Arguments: args})
	}
	stopReason := apiResp.DoneReason
	switch stopReason {
	case "length":
		stopReason = "max_tokens"
	case "stop", "":
		stopReason = "end"
	}
	// A reply carrying tool calls is a tool-use turn regardless of the daemon's
	// done_reason ("stop"), matching how the compat path maps finish_reason
	// "tool_calls" so the engine's tool loop branches identically on either path.
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}

	content := apiResp.Message.Content
	if verifyJSON && !looksLikeJSON(content) {
		code := llm.CodeSchemaDrift
		detail := "model returned 200 but the output is not valid JSON; the structured-output schema was not enforced"
		if stopReason == "max_tokens" {
			code = llm.CodeTruncated
			detail = "output was cut off before a complete JSON document (raise the token budget or simplify the schema)"
		}
		return nil, &llm.Error{
			Code: code,
			// PostJSON only decodes a 200 reply, so a drift error is always
			// reporting a nominally successful HTTP exchange.
			Status:  http.StatusOK,
			Message: "ollama: " + detail,
		}
	}

	return &llm.CompletionResponse{
		Content:          content,
		ReasoningContent: apiResp.Message.Thinking,
		ToolCalls:        toolCalls,
		Model:            apiResp.Model,
		StopReason:       stopReason,
		Usage: llm.Usage{
			InputTokens:  apiResp.PromptEvalCount,
			OutputTokens: apiResp.EvalCount,
		},
	}, nil
}

// looksLikeJSON reports whether s, trimmed of surrounding whitespace and an
// optional Markdown code fence, is a syntactically valid JSON document. Used to
// confirm Ollama actually constrained decoding to the supplied schema, so it
// checks strict validity (not the looser jsonrepair.Repair) - the point is to
// detect genuine schema drift, not to mask it.
func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(jsonrepair.StripCodeFence(s))
	return t != "" && json.Valid([]byte(t))
}
