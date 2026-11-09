package api

import "openfabrica/tools"

// manage_llm_proxy is the hosted chat-completion surface, split off from the
// former manage_streams omnibus. Creates /api/<path>/chat (SSE) and
// /api/<path>/complete (JSON) endpoints backed by the project's configured
// LLM provider.
//
// Cost-leak guards: rate_limit (per-IP per-minute) and daily_token_budget
// (per-endpoint per-UTC-day cap on input+output tokens, 0 = unlimited). The
// server-side counter rolls forward on the first hit each day; setting a
// non-zero budget caps end-user spend even when an endpoint is hammered.
//
// One arg struct per action; mergeLLMProxyOpts centralises the option map.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_llm_proxy",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult:          6000,
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Define hosted chat-completion endpoints at /api/<path>/chat (SSE) and /complete (JSON).",
		WhenToUse: `Use when the project wants to expose an LLM chat surface to its end users
without writing provider client code. Set system_prompt to constrain the
assistant, model_id to override the project default, and rate_limit (per-IP
requests/min) to throttle abuse. For bidirectional non-LLM traffic use a realtime
tool (manage_relay etc.); for one-way data streams use manage_sse.

Client contract for the page that calls it: /complete returns one JSON body (the default);
/chat is an SSE stream of token deltas. The request/response envelope, how to consume the
stream, and the "generate X" structured-output pattern live in the llm_endpoints guide
(auto-loaded with this tool) - for a JSON-parsed reply set response_schema and a high enough
max_tokens (a reply truncated at the cap is unparseable; a 502 means it hit max_tokens).`,
		Actions: []tools.Action{
			&LLMProxyCreate{},
			&LLMProxyUpdate{},
			&LLMProxyList{},
			&LLMProxyDelete{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type LLMProxyCreate struct {
	EndpointPath     string         `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash)."`
	RequiresAuth     bool           `arg:"requires_auth"          desc:"Connections must include a valid token."`
	RateLimit        int            `arg:"rate_limit"             desc:"Per-IP, per-minute request cap." default:"10"`
	DailyTokenBudget int            `arg:"daily_token_budget"     desc:"Per-endpoint per-UTC-day cap on input+output tokens (0 = unlimited). Strongly recommended for public endpoints." min:"0"`
	ModelID          string         `arg:"model_id"               desc:"LLM model id. Empty = project default."`
	SystemPrompt     string         `arg:"system_prompt"          desc:"LLM system prompt."`
	MaxHistory       int            `arg:"max_history"            desc:"Max conversation turns kept." default:"20"`
	MaxTokens        int            `arg:"max_tokens"             desc:"Max tokens in LLM response. Unset = 4096, or 8192 when response_schema is set (a truncated JSON reply is unparseable). Raise for long replies."`
	Temperature      float64        `arg:"temperature"            desc:"LLM sampling temperature. A value of exactly 0 is treated as unset (use e.g. 0.05 for near-deterministic output); any other value is passed through to the provider unchanged." default:"0.7"`
	CORSOrigins      string         `arg:"cors_origins"           desc:"CSV CORS origins, '*' for any."`
	ResponseSchema   map[string]any `arg:"response_schema" desc:"JSON Schema document. When set, the endpoint forces the model to emit JSON conforming to it (native structured output) so the client can JSON.parse the reply reliably. Pass this for any 'generate X' feature whose UI parses the reply's content as JSON (/complete answers {content, model, stop_reason, usage} - JSON.parse(body.content)); leave unset for free-form text/chat. Bumps the max_tokens default to 8192."`
	KnowledgeBase    string         `arg:"knowledge_base"         desc:"Bind this chat endpoint to a manage_knowledge base (by name) for retrieval-augmented answers. The proxy retrieves the most relevant chunks per request and grounds the reply in them. Empty = plain chat."`
}

func (a *LLMProxyCreate) Doc() string {
	return "Create an LLM chat-completion endpoint. Create-only: if the path already exists this is a no-op that changes nothing - use action=update to change an existing endpoint."
}
func (a *LLMProxyCreate) Example() any {
	return &LLMProxyCreate{EndpointPath: "chat", SystemPrompt: "You are a helpful assistant."}
}
func (a *LLMProxyCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateLLM(ctx, llmEndpointConfig{
		Path:             a.EndpointPath,
		RequiresAuth:     &a.RequiresAuth,
		RateLimit:        a.RateLimit,
		DailyTokenBudget: &a.DailyTokenBudget,
		ModelID:          a.ModelID,
		SystemPrompt:     a.SystemPrompt,
		MaxHistory:       a.MaxHistory,
		MaxTokens:        a.MaxTokens,
		Temperature:      a.Temperature,
		CORSOrigins:      a.CORSOrigins,
		ResponseSchema:   a.ResponseSchema,
		KnowledgeBase:    a.KnowledgeBase,
	})
}

// --- update -----------------------------------------------------------------

type LLMProxyUpdate struct {
	EndpointPath     string         `arg:"endpoint_path,required" desc:"LLM endpoint path to update."`
	RequiresAuth     *bool          `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	RateLimit        int            `arg:"rate_limit"             desc:"Replace the per-IP per-minute cap."`
	DailyTokenBudget *int           `arg:"daily_token_budget"     desc:"Replace the daily token budget; 0 lifts it (unlimited). Omit to leave unchanged." min:"0"`
	ModelID          string         `arg:"model_id"               desc:"Replace the model id."`
	SystemPrompt     string         `arg:"system_prompt"          desc:"Replace the system prompt."`
	MaxHistory       int            `arg:"max_history"            desc:"Replace max conversation turns."`
	MaxTokens        int            `arg:"max_tokens"             desc:"Replace max response tokens."`
	Temperature      float64        `arg:"temperature"            desc:"Replace the sampling temperature; a value of exactly 0 is treated as unset (provider default)."`
	CORSOrigins      string         `arg:"cors_origins"           desc:"Replace CSV CORS origins."`
	ResponseSchema   map[string]any `arg:"response_schema" desc:"Replace the JSON Schema that constrains responses (native structured output). Pass an empty object {} to clear it and return to free-form text."`
	KnowledgeBase    string         `arg:"knowledge_base"         desc:"Bind (or rebind) this endpoint to a manage_knowledge base by name for retrieval-augmented answers. Pass \"none\" to unbind it back to plain chat."`
}

func (a *LLMProxyUpdate) Doc() string { return "Update an existing LLM endpoint's options." }
func (a *LLMProxyUpdate) Example() any {
	return &LLMProxyUpdate{EndpointPath: "chat", RateLimit: 5}
}
func (a *LLMProxyUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateLLM(ctx, llmEndpointConfig{
		Path:             a.EndpointPath,
		RequiresAuth:     a.RequiresAuth,
		RateLimit:        a.RateLimit,
		DailyTokenBudget: a.DailyTokenBudget,
		ModelID:          a.ModelID,
		SystemPrompt:     a.SystemPrompt,
		MaxHistory:       a.MaxHistory,
		MaxTokens:        a.MaxTokens,
		Temperature:      a.Temperature,
		CORSOrigins:      a.CORSOrigins,
		ResponseSchema:   a.ResponseSchema,
		KnowledgeBase:    a.KnowledgeBase,
	})
}

// --- list -------------------------------------------------------------------

type LLMProxyList struct{}

func (a *LLMProxyList) Doc() string    { return "List every LLM endpoint." }
func (a *LLMProxyList) ReadOnly() bool { return true }
func (a *LLMProxyList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListLLM(ctx)
}

// --- delete -----------------------------------------------------------------

type LLMProxyDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"LLM endpoint path."`
}

func (a *LLMProxyDelete) Doc() string { return "Delete an LLM endpoint." }
func (a *LLMProxyDelete) Example() any {
	return &LLMProxyDelete{EndpointPath: "chat"}
}
func (a *LLMProxyDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteLLM(ctx, a.EndpointPath)
}
