package api

// Typed configuration carriers for the endpoint create/update handlers whose
// create and update actions MERGE with tri-state semantics (a *bool / *int
// field distinguishes "omitted" from "explicitly zero", so update can turn a
// setting off, not just on). create passes addresses of its plain fields
// (always non-nil); update passes its pointer fields straight through.
//
// Handlers whose create/update pair is a plain field-for-field copy take the
// Action struct directly instead (see epCreateAPI / feedCreate) - a carrier
// there would be a third copy of the same field list.

// llmEndpointConfig carries the options for an LLM proxy endpoint
// (manage_llm_proxy create/update). create reads every field (RequiresAuth is
// non-nil, its default false); update patches only the fields that are set, and
// RequiresAuth is tri-state (nil = "not provided") so update can turn the auth
// requirement off, not just on - the same reasoning as wsEndpointConfig below.
type llmEndpointConfig struct {
	Path         string
	RequiresAuth *bool
	RateLimit    int
	// DailyTokenBudget is tri-state (nil = "not provided"): 0 is the documented
	// "unlimited" value, so a plain int could never lift a budget once set.
	DailyTokenBudget *int
	ModelID          string
	SystemPrompt     string
	MaxHistory       int
	MaxTokens        int
	Temperature      float64
	CORSOrigins      string
	ResponseSchema   map[string]any
	KnowledgeBase    string
}

// agentEndpointConfig carries the options for a hosted agent endpoint
// (manage_agent create/update). Same tri-state discipline as llmEndpointConfig:
// create supplies non-nil RequiresAuth/DailyTokenBudget/AllowDataQuery, update
// passes them through so a setting can be turned off, not only on.
type agentEndpointConfig struct {
	Path         string
	RequiresAuth *bool
	RateLimit    int
	// DailyTokenBudget is tri-state (nil = "not provided"): 0 is the documented
	// "unlimited" value, so a plain int could never lift a budget once set.
	DailyTokenBudget *int
	ModelID          string
	SystemPrompt     string
	MaxSteps         int
	MaxTokens        int
	Temperature      float64
	CORSOrigins      string
	// Functions is the CSV of manage_function endpoint paths the agent may call
	// as tools (stored as a JSON array on the row). Empty = the agent has no
	// function tools.
	Functions string
	// AllowDataQuery is tri-state so update can withdraw the read-only query tool.
	AllowDataQuery *bool
	// MaxSubagents caps total sub-agent spawns per run; it is tri-state (nil =
	// "not provided") because 0 is the meaningful "sub-agents off" value, so a
	// plain int could never turn them back off on update. MaxDepth caps nesting.
	MaxSubagents *int
	MaxDepth     int
	// AllowHTTPFetch is tri-state so update can withdraw the outbound-fetch tool.
	AllowHTTPFetch *bool
}

// wsEndpointConfig carries the options for a WebSocket endpoint (the create/update
// actions of the per-model realtime tools: manage_relay / manage_tick /
// manage_authoritative / manage_matchmaking / manage_signaling). Zero-valued
// optional fields mean "unset" and fall back to the per-mode defaults in defaults.go.
type wsEndpointConfig struct {
	Path string
	// RequiresAuth is tri-state (nil = "not provided"): create always passes a
	// non-nil value (its default false), while update passes nil when the caller
	// omits requires_auth. This lets update set it to false explicitly - under the
	// old plain-bool truthy-merge, false was indistinguishable from "unset", so an
	// over-gated endpoint could never be opened back up.
	RequiresAuth  *bool
	WriteToTable  string
	RoomColumn    string
	TenantColumn  string
	Mode          string // "" | chat | signaling
	TickHz        int
	WSInboundRate int
	MaxMsgSize    int
	E2EE          bool
	Ephemeral     bool
	// KeepHistory opts an e2ee room out of the ephemeral default (relay only): its
	// persisted ciphertext survives the room emptying, which an archive:true chat
	// history needs. On update it turns ephemeral off.
	KeepHistory bool
	RoomGated   bool
	Matchmaking bool
	GroupSize   int
	// MaxClients is tri-state (nil = "not provided") for the same reason as
	// RequiresAuth: 0 is the documented "unlimited" value, so a plain int could
	// never express it on update - a cap could be raised but never lifted.
	MaxClients *int
	MessageTTL int
	// Model is the realtime tool that owns the endpoint ("relay" | "tick" | "authoritative"
	// | "matchmaking" | "signaling"; wsendpoint.Model*). Create stores it; update refuses an
	// endpoint another model owns, so one tool's update can't silently convert or break
	// another model's endpoint. "" skips the ownership check.
	Model string
	// Authoritative marks an endpoint whose state a server reducer owns. The
	// reducer itself is attached later by set_reducer, so this is what tells the
	// create result which delivery model to document: an authoritative endpoint
	// pushes {_type:'state'} views and never emits the tick snapshot, even when
	// tick_hz is set (there it only drives @tick events into reduce()).
	Authoritative bool
}
