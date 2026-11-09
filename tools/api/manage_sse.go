package api

import "openfabrica/tools"

// manage_sse is the Server-Sent Events transport surface, split off from the
// former manage_streams omnibus. Use for one-way server -> client event
// broadcasts from a table (live feeds, dashboards, notifications). For
// bidirectional traffic use a realtime tool (manage_relay / manage_tick / etc.);
// for hosted chat-completion use manage_llm_proxy.
//
// One arg struct per action - create/update share the option surface; the
// mergeSSEOpts helper centralises the args-map shape.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_sse",
		Category:           "api",
		FullResultInEvents: true,
		OneLiner:           "Define Server-Sent Events endpoints served at GET /api/<path>/stream - one-way server -> client push of row-change events.",
		WhenToUse: `Use when the client only needs to read updates as they happen (live feed,
dashboard tile, notification ticker). event_types filters which row-change
events are pushed; table_name optionally scopes the stream to one source
table - the PHYSICAL TABLE NAME (the table_name you passed to create_table),
not the endpoint's route. Omit table_name to push every data event of the
chosen types across the whole project. Per-ROW scoping is inherited, never set here:
a subscriber receives a row only if the endpoint that published it would have
let them read it, so scope the create_api endpoint and the stream follows.
This tool implements the plan's create_stream endpoint action.`,
		Actions: []tools.Action{
			&SSECreate{},
			&SSEUpdate{},
			&SSEList{},
			&SSEDelete{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type SSECreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash)."`
	RequiresAuth bool   `arg:"requires_auth"          desc:"Connections must include a valid token."`
	TableName    string `arg:"table_name"             desc:"Optional: scope to one source table - the PHYSICAL TABLE NAME (what you passed to create_table), NOT the endpoint route. An endpoint at orders/recent over table orders wants table_name=orders; a name that is not a table is refused. Omit to push every project data event of these types."`
	EventTypes   []any  `arg:"event_types"            desc:"Event types to broadcast (default: data.insert, data.update, data.delete)." items:"string"`
}

func (a *SSECreate) Doc() string { return "Create an SSE endpoint sourced from a dynamic table." }
func (a *SSECreate) Example() any {
	return &SSECreate{EndpointPath: "feed", TableName: "posts"}
}
func (a *SSECreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateStream(ctx, a.EndpointPath, a.RequiresAuth, a.TableName, a.EventTypes)
}

// --- update -----------------------------------------------------------------

type SSEUpdate struct {
	EndpointPath string  `arg:"endpoint_path,required" desc:"SSE endpoint path to update."`
	RequiresAuth *bool   `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	TableName    *string `arg:"table_name"             desc:"Replace the source-table scope - the PHYSICAL TABLE NAME, not the endpoint route; empty string widens to all tables. Omit to leave unchanged."`
	EventTypes   []any   `arg:"event_types"            desc:"Replace the event-type filter." items:"string"`
}

func (a *SSEUpdate) Doc() string { return "Update an existing SSE endpoint's options." }
func (a *SSEUpdate) Example() any {
	table := "posts"
	return &SSEUpdate{EndpointPath: "feed", TableName: &table}
}
func (a *SSEUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateStream(ctx, a.EndpointPath, a.RequiresAuth, a.TableName, a.EventTypes)
}

// --- list -------------------------------------------------------------------

type SSEList struct{}

func (a *SSEList) Doc() string    { return "List every SSE endpoint." }
func (a *SSEList) ReadOnly() bool { return true }
func (a *SSEList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epListStream(ctx)
}

// --- delete -----------------------------------------------------------------

type SSEDelete struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"SSE endpoint path."`
}

func (a *SSEDelete) Doc() string { return "Delete an SSE endpoint." }
func (a *SSEDelete) Example() any {
	return &SSEDelete{EndpointPath: "feed"}
}
func (a *SSEDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epDeleteStream(ctx, a.EndpointPath)
}
