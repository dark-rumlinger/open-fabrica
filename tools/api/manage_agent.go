package api

import "openfabrica/tools"

// manage_agent is the multi-step sibling of manage_llm_proxy. Where the proxy
// returns one completion, an agent endpoint runs a bounded tool-calling loop
// (internal/agentrt): the model plans, calls the app's OWN capabilities, reads
// the results, and loops until it answers or a cap is hit. It mounts
// POST /api/<path>/run and stores its config in of_agent_endpoints.
//
// The agent's tools are allowlisted per endpoint - the app's manage_function
// endpoints (by path) and, opt-in, a read-only data-query tool - never the
// platform build tools. Cost is bounded by max_steps and a per-run
// daily_token_budget, and requires_auth is on by default: an agent spends the
// owner's tokens across several calls, so a public one is opt-in.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_agent",
		Category: "api",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult:          8000,
		FullResultInEvents: true,
		OneLiner:           "Define a hosted agent endpoint at /api/<path>/run: a bounded tool-calling loop over the app's own functions and data.",
		WhenToUse: `Use when the app needs an assistant that DECIDES and ACTS over several steps:
it calls the app's own manage_function endpoints (and, with allow_data_query, reads
tables) in a loop until it can answer, instead of returning one reply. Prefer
manage_llm_proxy for a plain chatbot or a single "generate X" call: an agent makes
many model calls per request, so it defaults to requires_auth on and wants a
daily_token_budget. List each callable endpoint in ` + "`functions`" + ` and describe
their jobs in system_prompt so the model knows when to call them.`,
		Actions: []tools.Action{
			&AgentCreate{},
			&AgentUpdate{},
			&AgentList{},
			&AgentDelete{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

// AgentCreate creates an agent endpoint. The max tags on the caps below mirror
// the runtime ceilings in internal/agentrt (MaxStepsCeiling, MaxDepthCeiling,
// MaxSubagentsCeiling), so an out-of-range value is rejected here instead of
// silently clamped at run time.
type AgentCreate struct {
	EndpointPath     string  `arg:"endpoint_path,required" desc:"Path suffix naming the RESOURCE (no /api/, no leading slash). Serves at /api/<path>/run, so do not end it in \"run\"."`
	SystemPrompt     string  `arg:"system_prompt"          desc:"Instructions that define the agent's job, tone, and when to use each tool."`
	ModelID          string  `arg:"model_id"               desc:"LLM model id. Empty = project default."`
	Functions        string  `arg:"functions"              desc:"CSV of manage_function endpoint paths the agent may call as tools. Empty = no function tools."`
	AllowDataQuery   bool    `arg:"allow_data_query"       desc:"Expose a read-only SELECT tool over the project tables (no writes, no PASSWORD columns)."`
	AllowHTTPFetch   bool    `arg:"allow_http_fetch"       desc:"Expose an SSRF-guarded outbound HTTP fetch tool (GET/POST to external URLs)."`
	MaxSubagents     int     `arg:"max_subagents"          desc:"Total sub-agents the run may spawn (0 = off, hard cap 32); enables the spawn_subagent delegation tool." min:"0" max:"32"`
	MaxDepth         int     `arg:"max_depth"              desc:"How deep spawned sub-agents may nest (hard cap 3)." default:"1" max:"3"`
	MaxSteps         int     `arg:"max_steps"              desc:"Tool-calling iterations before the run stops (hard cap 40)." default:"8" max:"40"`
	MaxTokens        int     `arg:"max_tokens"             desc:"Max tokens per model call." default:"4096"`
	Temperature      float64 `arg:"temperature"            desc:"LLM sampling temperature. A value of exactly 0 is treated as unset." default:"0.7"`
	RequiresAuth     bool    `arg:"requires_auth"          desc:"Require a valid token on /run. Agents keep this on unless you pass false." default:"true"`
	RateLimit        int     `arg:"rate_limit"             desc:"Per-IP, per-minute request cap." default:"10"`
	DailyTokenBudget int     `arg:"daily_token_budget"     desc:"Per-endpoint per-UTC-day cap on the whole run's input+output tokens (0 = unlimited). Strongly recommended for public endpoints." min:"0"`
	CORSOrigins      string  `arg:"cors_origins"           desc:"CSV CORS origins, '*' for any."`
}

func (a *AgentCreate) Doc() string {
	return "Create an agent endpoint. Create-only: if the path already exists this is a no-op that changes nothing - use action=update to change an existing endpoint."
}
func (a *AgentCreate) Example() any {
	return &AgentCreate{EndpointPath: "assistant", SystemPrompt: "You help users manage their orders. Use the fn_lookup_order tool to fetch an order before answering.", Functions: "lookup_order"}
}
func (a *AgentCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateAgent(ctx, agentEndpointConfig{
		Path:             a.EndpointPath,
		RequiresAuth:     &a.RequiresAuth,
		RateLimit:        a.RateLimit,
		DailyTokenBudget: &a.DailyTokenBudget,
		ModelID:          a.ModelID,
		SystemPrompt:     a.SystemPrompt,
		MaxSteps:         a.MaxSteps,
		MaxTokens:        a.MaxTokens,
		Temperature:      a.Temperature,
		CORSOrigins:      a.CORSOrigins,
		Functions:        a.Functions,
		AllowDataQuery:   &a.AllowDataQuery,
		AllowHTTPFetch:   &a.AllowHTTPFetch,
		MaxSubagents:     &a.MaxSubagents,
		MaxDepth:         a.MaxDepth,
	})
}

// --- update -----------------------------------------------------------------

type AgentUpdate struct {
	EndpointPath     string  `arg:"endpoint_path,required" desc:"Agent endpoint path to update."`
	SystemPrompt     string  `arg:"system_prompt"          desc:"Replace the system prompt."`
	ModelID          string  `arg:"model_id"               desc:"Replace the model id."`
	Functions        string  `arg:"functions"              desc:"Replace the CSV of manage_function endpoint paths the agent may call. Pass \"none\" to remove every function tool."`
	AllowDataQuery   *bool   `arg:"allow_data_query"       desc:"Toggle the read-only data-query tool on (true) or off (false). Omit to leave unchanged."`
	AllowHTTPFetch   *bool   `arg:"allow_http_fetch"       desc:"Toggle the outbound HTTP fetch tool on (true) or off (false). Omit to leave unchanged."`
	MaxSubagents     *int    `arg:"max_subagents"          desc:"Replace the sub-agent spawn cap; 0 turns sub-agents off (hard cap 32). Omit to leave unchanged." min:"0" max:"32"`
	MaxDepth         int     `arg:"max_depth"              desc:"Replace the sub-agent nesting depth (hard cap 3)."  max:"3"`
	MaxSteps         int     `arg:"max_steps"              desc:"Replace the tool-calling iteration cap (hard cap 40)." max:"40"`
	MaxTokens        int     `arg:"max_tokens"             desc:"Replace max tokens per model call."`
	Temperature      float64 `arg:"temperature"            desc:"Replace the sampling temperature; a value of exactly 0 is treated as unset."`
	RequiresAuth     *bool   `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	RateLimit        int     `arg:"rate_limit"             desc:"Replace the per-IP per-minute cap."`
	DailyTokenBudget *int    `arg:"daily_token_budget"     desc:"Replace the daily token budget; 0 lifts it (unlimited). Omit to leave unchanged." min:"0"`
	CORSOrigins      string  `arg:"cors_origins"           desc:"Replace CSV CORS origins. Pass \"none\" to clear them."`
}

func (a *AgentUpdate) Doc() string { return "Update an existing agent endpoint's options." }
func (a *AgentUpdate) Example() any {
	return &AgentUpdate{EndpointPath: "assistant", MaxSteps: 12}
}
func (a *AgentUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateAgent(ctx, agentEndpointConfig{
		Path:             a.EndpointPath,
		RequiresAuth:     a.RequiresAuth,
		RateLimit:        a.RateLimit,
		DailyTokenBudget: a.DailyTokenBudget,
		ModelID:          a.ModelID,
		SystemPrompt:     a.SystemPrompt,
		MaxSteps:         a.MaxSteps,
		MaxTokens:        a.MaxTokens,
		Temperature:      a.Temperature,
		CORSOrigins:      a.CORSOrigins,
		Functions:        a.Functions,
		AllowDataQuery:   a.AllowDataQuery,
		AllowHTTPFetch:   a.AllowHTTPFetch,
		MaxSubagents:     a.MaxSubagents,
		MaxDepth:         a.MaxDepth,
	})
}

// --- list -------------------------------------------------------------------

type AgentList struct{}

func (a *AgentList) Doc() string    { return "List every agent endpoint." }
func (a *AgentList) ReadOnly() bool { return true }
func (a *AgentList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListAgent(ctx)
}

// --- delete -----------------------------------------------------------------

type AgentDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Agent endpoint path."`
}

func (a *AgentDelete) Doc() string { return "Delete an agent endpoint." }
func (a *AgentDelete) Example() any {
	return &AgentDelete{EndpointPath: "assistant"}
}
func (a *AgentDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteAgent(ctx, a.EndpointPath)
}
