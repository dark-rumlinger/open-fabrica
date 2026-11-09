package api

import (
	"openfabrica/internal/wsendpoint"
	"openfabrica/tools"
)

// manage_tick is the TICK realtime model: the server coalesces each room to ONE
// snapshot per tick, carrying the clients who sent since the last one. It is the
// right model for CONTINUOUS shared state (cursors, positions) where only the
// latest value matters. Discrete append-only events (chat lines, strokes) ride
// the same endpoint through net.event, which is delivered whole rather than
// folded into the snapshot. Shares the of_ws_endpoints runtime.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_tick",
		RealtimeModel:      "tick",
		Category:           "api",
		FullResultInEvents: true,
		MaxResult:          16000,
		OneLiner:           "Realtime WebSocket TICK endpoint for multiplayer movement: server-coalesced snapshots of continuous state (cursors, positions).",
		WhenToUse: `Use for CONTINUOUS shared state that streams at high frequency and
where only the latest value matters: live cursors, avatar/entity positions, a
shared canvas's pointer. Set tick_hz to 20/30/60; the server coalesces each
room to one snapshot per tick. Discrete append-only events (chat lines, drawn
strokes, "player scored") belong on THIS endpoint too, so an app with both needs
one tick endpoint, not a separate relay. Cheat-sensitive / turn-based / hidden-info games ->
manage_authoritative. Read/delete with manage_realtime.`,
		Actions: []tools.Action{
			&TickCreate{},
			&TickUpdate{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type TickCreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash, no :param)."`
	TickHz       int    `arg:"tick_hz,required"       desc:"Snapshot rate in Hz - 20/30/60 recommended, up to 120. The server coalesces each room to this many snapshots/sec. (Here tick_hz is a SNAPSHOT rate; the same arg on manage_authoritative is a SIM rate that feeds @tick into the reducer and emits no snapshot.)" max:"120"`
	RequiresAuth bool   `arg:"requires_auth"          desc:"Connections must include a valid token (connect() auto-carries it; a raw socket cannot authenticate)."`
	WriteToTable string `arg:"write_to_table"         desc:"Table to auto-insert this endpoint's DISCRETE messages into (net.event: chat lines, strokes), for history / late-join. Coalesced net.send state is never persisted, so a movement stream costs no writes and needs no filtering by hand. Declare the durable frame's own column NOT NULL and any other frame shape is skipped quietly."`
	RoomColumn   string `arg:"room_column"            desc:"Column name used for room scoping; also lets a client switch rooms live by sending that exact field."`
	TenantColumn string `arg:"tenant_column"          desc:"Column auto-stamped from the JWT's tenant_id (requires requires_auth)."`
	Ephemeral    bool   `arg:"ephemeral"              desc:"Throwaway rooms: when a room empties the server deletes this endpoint's persisted rows for that room (needs write_to_table + room_column). Set it for per-match/per-session rooms, or their history grows forever."`
	MessageTTL   int    `arg:"message_ttl_seconds"    desc:"Disappearing messages: persisted rows older than this many seconds are swept server-side (needs write_to_table). 0 = keep forever; max 31536000 (1 year)."`
	MaxClients   int    `arg:"max_clients"            desc:"Cap on participants per room (0 = unlimited). Overflow joiners connect as read-only spectators."`
	MaxMsgSize   int    `arg:"max_msg_size"           desc:"Hard cap on single inbound frame bytes, max 1048576 (1 MiB)." default:"16384"`
}

func (a *TickCreate) Doc() string {
	return "Create a tick endpoint (server-coalesced snapshots for continuous shared state). tick_hz is required (20/30/60 typical; up to 120)."
}
func (a *TickCreate) Example() any {
	return &TickCreate{EndpointPath: "arena", TickHz: 30}
}
func (a *TickCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if a.TickHz <= 0 {
		return tools.Fail("tick_hz is required and must be > 0 (20/30/60 typical; up to 120). For a discrete-message channel (chat/presence) use manage_relay instead."), nil
	}
	return epCreateWebSocket(ctx, wsEndpointConfig{
		Model:        wsendpoint.ModelTick,
		Path:         a.EndpointPath,
		RequiresAuth: &a.RequiresAuth,
		TickHz:       a.TickHz,
		WriteToTable: a.WriteToTable,
		RoomColumn:   a.RoomColumn,
		TenantColumn: a.TenantColumn,
		Ephemeral:    a.Ephemeral,
		MessageTTL:   a.MessageTTL,
		MaxClients:   &a.MaxClients,
		MaxMsgSize:   a.MaxMsgSize,
	})
}

// --- update -----------------------------------------------------------------

type TickUpdate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Tick endpoint path to update."`
	TickHz       int    `arg:"tick_hz"                desc:"Replace the snapshot rate (20/30/60 typical; up to 120)." max:"120"`
	RequiresAuth *bool  `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	WriteToTable string `arg:"write_to_table"         desc:"Replace the table this endpoint's discrete (net.event) messages persist to."`
	RoomColumn   string `arg:"room_column"            desc:"Replace the room-scope column."`
	TenantColumn string `arg:"tenant_column"          desc:"Replace the tenant column."`
	Ephemeral    bool   `arg:"ephemeral"              desc:"Turn on auto-prune of this endpoint's rows when a room empties. Only flips on."`
	MessageTTL   int    `arg:"message_ttl_seconds"    desc:"Set the disappearing-messages TTL in seconds (>0, max 31536000 = 1 year)."`
	MaxClients   *int   `arg:"max_clients"            desc:"Replace the per-room participant cap; 0 lifts the cap (unlimited). Omit to leave unchanged."`
	MaxMsgSize   int    `arg:"max_msg_size"           desc:"Replace single-frame byte cap, max 1048576 (1 MiB)."`
}

func (a *TickUpdate) Doc() string { return "Update an existing tick endpoint's options." }
func (a *TickUpdate) Example() any {
	return &TickUpdate{EndpointPath: "arena", TickHz: 60}
}
func (a *TickUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateWebSocket(ctx, wsEndpointConfig{
		Model:        wsendpoint.ModelTick,
		Path:         a.EndpointPath,
		RequiresAuth: a.RequiresAuth,
		TickHz:       a.TickHz,
		WriteToTable: a.WriteToTable,
		RoomColumn:   a.RoomColumn,
		TenantColumn: a.TenantColumn,
		Ephemeral:    a.Ephemeral,
		MessageTTL:   a.MessageTTL,
		MaxClients:   a.MaxClients,
		MaxMsgSize:   a.MaxMsgSize,
	})
}
