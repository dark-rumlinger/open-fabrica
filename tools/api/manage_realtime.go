package api

import "openfabrica/tools"

// manage_realtime is the shared read/delete surface across every realtime model
// (relay / tick / authoritative / matchmaking / signaling), which are all rows
// in of_ws_endpoints. Creation lives on the model-specific tools; inspection and
// removal are model-agnostic, so they live here once rather than duplicated five
// times. `get` returns the full config plus the reducer source for authoritative
// endpoints (manage_authoritative also exposes get for the reducer-fix workflow).

func init() {
	tools.Define(tools.Tool{
		Name: "manage_realtime",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult:          16000,
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Inspect or remove realtime endpoints (list, get config+reducer, delete) across all models.",
		WhenToUse: `Use to list every realtime endpoint, read one endpoint's full config
(get - includes the reducer source for authoritative endpoints, so read it
before fixing a reducer), or delete one. CREATE endpoints with the
model-specific tools: manage_relay (chat/presence), manage_tick (continuous
state), manage_authoritative (reducer games), manage_matchmaking (auto-pairing),
manage_signaling (WebRTC).`,
		Actions: []tools.Action{
			&RealtimeList{},
			&RealtimeGet{},
			&RealtimeDelete{},
		},
		Summarize: EpSummarize,
	})
}

// --- list -------------------------------------------------------------------

type RealtimeList struct{}

func (a *RealtimeList) Doc() string {
	return "List every realtime endpoint (all models) with its config."
}
func (a *RealtimeList) ReadOnly() bool { return true }
func (a *RealtimeList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListWebSocket(ctx)
}

// --- get --------------------------------------------------------------------

type RealtimeGet struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Realtime endpoint to fetch the full config for, INCLUDING the reducer source (authoritative endpoints). Read the reducer here, fix the exact line, then re-attach with manage_authoritative set_reducer."`
}

func (a *RealtimeGet) Doc() string {
	return "Fetch one endpoint's full config plus its reducer source (if authoritative)."
}
func (a *RealtimeGet) ReadOnly() bool { return true }
func (a *RealtimeGet) Example() any   { return &RealtimeGet{EndpointPath: "game"} }
func (a *RealtimeGet) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epGetWebSocket(ctx, a.EndpointPath)
}

// --- delete -----------------------------------------------------------------

type RealtimeDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Realtime endpoint path to delete."`
}

func (a *RealtimeDelete) Doc() string  { return "Delete a realtime endpoint (any model)." }
func (a *RealtimeDelete) Example() any { return &RealtimeDelete{EndpointPath: "chat"} }
func (a *RealtimeDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteWebSocket(ctx, a.EndpointPath)
}
