package api

import (
	"openfabrica/internal/wsendpoint"
	"openfabrica/tools"
)

// manage_relay is the RELAY realtime model: a transparent bidirectional JSON
// broadcast channel with rooms and presence, for chat, online lists, and
// discrete append-only events. It is one of the model-explicit realtime tools
// that replaced the overloaded manage_websocket: relay never carries a server
// tick (tick_hz is fixed 0), a reducer, or WebRTC signaling, so those footguns
// are simply not representable here. All realtime models share the same runtime
// (epCreateWebSocket / of_ws_endpoints); only the exposed surface differs.

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_relay",
		RealtimeModel:      "relay",
		Category:           "api",
		FullResultInEvents: true,
		MaxResult:          16000,
		OneLiner:           "Realtime WebSocket RELAY endpoint: chat, presence, and discrete broadcast messages (no server tick).",
		WhenToUse: `Use when a room carries ONLY discrete messages and no continuous
state: chat, presence / "who's online", notifications between connected clients,
or append-only collaborative events. The discrete lane exists on EVERY realtime
model, so a room that later needs continuous state moves to manage_tick without
reworking how it sends messages. Choosing among the realtime models is the
realtime_signaling guide's decision facet, which PLAN locks. One-way
server->client push is manage_sse; read/delete any realtime endpoint with
manage_realtime.`,
		Actions: []tools.Action{
			&RelayCreate{},
			&RelayUpdate{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type RelayCreate struct {
	EndpointPath  string `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash, no :param)."`
	RequiresAuth  bool   `arg:"requires_auth"          desc:"Connections must include a valid token (connect() auto-carries it as ?token=; a raw socket cannot authenticate)."`
	WriteToTable  string `arg:"write_to_table"         desc:"Table to auto-insert incoming messages into (for history / late-join), mapping frame fields to columns. When more than one frame shape flows through, the table's own shape filters: a frame that cannot fill a NOT NULL column is skipped quietly, so declaring the durable frame's column NOT NULL persists only that shape. Make every column NULLable and every frame lands, junk rows included. On e2ee=true only the {iv,ct} ciphertext envelope is persisted - and NOT with a ratcheting group session, whose per-message index is not part of that envelope, so replayed history would decrypt to nothing."`
	RoomColumn    string `arg:"room_column"            desc:"Column name used for room scoping; also lets a client switch rooms live by sending that exact field."`
	TenantColumn  string `arg:"tenant_column"          desc:"Column auto-stamped from the JWT's tenant_id (requires requires_auth)."`
	E2EE          bool   `arg:"e2ee"                   desc:"Enforce end-to-end encryption: the relay drops any plaintext body, persists only ciphertext, and emits js/e2ee.js (import { session } from 'e2ee'). See the e2ee guide."`
	Ephemeral     bool   `arg:"ephemeral"              desc:"Throwaway rooms: when a room empties the relay auto-deletes this endpoint's persisted rows for that room (needs write_to_table + room_column). ON by default when e2ee=true unless keep_history=true."`
	KeepHistory   bool   `arg:"keep_history"           desc:"e2ee rooms that keep scroll-back history (the client opens its session with archive:true): keep persisted ciphertext when a room empties instead of the e2ee ephemeral default."`
	RoomGated     bool   `arg:"room_gated"             desc:"Gate room ENTRY on a per-room ?join=<token> (the first peer mints it, returned in welcome as _joinToken). Use so knowing the room name is not enough to join."`
	MessageTTL    int    `arg:"message_ttl_seconds"    desc:"Disappearing messages: persisted rows older than this many seconds are swept server-side (needs write_to_table). 0 = keep forever; max 31536000 (1 year)."`
	MaxClients    int    `arg:"max_clients"            desc:"Cap on participants per room (0 = unlimited). Overflow joiners connect as read-only spectators (welcome._role='spectator')."`
	MaxMsgSize    int    `arg:"max_msg_size"           desc:"Hard cap on single inbound frame bytes, max 1048576 (1 MiB)." default:"16384"`
	WSInboundRate int    `arg:"ws_inbound_rate"        desc:"Per-client inbound text msg/sec cap." default:"120"`
}

func (a *RelayCreate) Doc() string {
	return "Create a relay (chat/presence/discrete-message) endpoint."
}
func (a *RelayCreate) Example() any {
	return &RelayCreate{EndpointPath: "chat"}
}
func (a *RelayCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateWebSocket(ctx, wsEndpointConfig{
		Model:         wsendpoint.ModelRelay,
		Path:          a.EndpointPath,
		RequiresAuth:  &a.RequiresAuth,
		WriteToTable:  a.WriteToTable,
		RoomColumn:    a.RoomColumn,
		TenantColumn:  a.TenantColumn,
		E2EE:          a.E2EE,
		Ephemeral:     a.Ephemeral,
		KeepHistory:   a.KeepHistory,
		RoomGated:     a.RoomGated,
		MessageTTL:    a.MessageTTL,
		MaxClients:    &a.MaxClients,
		MaxMsgSize:    a.MaxMsgSize,
		WSInboundRate: a.WSInboundRate,
	})
}

// --- update -----------------------------------------------------------------

type RelayUpdate struct {
	EndpointPath  string `arg:"endpoint_path,required" desc:"Relay endpoint path to update."`
	RequiresAuth  *bool  `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	WriteToTable  string `arg:"write_to_table"         desc:"Replace the persistence table."`
	RoomColumn    string `arg:"room_column"            desc:"Replace the room-scope column."`
	TenantColumn  string `arg:"tenant_column"          desc:"Replace the tenant column."`
	E2EE          bool   `arg:"e2ee"                   desc:"Turn on end-to-end encryption enforcement (see create). Only flips on."`
	Ephemeral     bool   `arg:"ephemeral"              desc:"Turn on auto-prune of this endpoint's rows when a room empties."`
	KeepHistory   bool   `arg:"keep_history"           desc:"Turn auto-prune OFF so a room's rows survive it emptying (an e2ee room with archive:true history)."`
	RoomGated     bool   `arg:"room_gated"             desc:"Turn on the per-room access-token entry gate. Only flips on."`
	MessageTTL    int    `arg:"message_ttl_seconds"    desc:"Set the disappearing-messages TTL in seconds (>0, max 31536000 = 1 year)."`
	MaxClients    *int   `arg:"max_clients"            desc:"Replace the per-room participant cap; 0 lifts the cap (unlimited). Omit to leave unchanged."`
	MaxMsgSize    int    `arg:"max_msg_size"           desc:"Replace single-frame byte cap, max 1048576 (1 MiB)."`
	WSInboundRate int    `arg:"ws_inbound_rate"        desc:"Replace per-client inbound msg/sec cap."`
}

func (a *RelayUpdate) Doc() string { return "Update an existing relay endpoint's options." }
func (a *RelayUpdate) Example() any {
	eight := 8
	return &RelayUpdate{EndpointPath: "chat", MaxClients: &eight}
}
func (a *RelayUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epUpdateWebSocket(ctx, wsEndpointConfig{
		Model:         wsendpoint.ModelRelay,
		Path:          a.EndpointPath,
		RequiresAuth:  a.RequiresAuth,
		WriteToTable:  a.WriteToTable,
		RoomColumn:    a.RoomColumn,
		TenantColumn:  a.TenantColumn,
		E2EE:          a.E2EE,
		Ephemeral:     a.Ephemeral,
		KeepHistory:   a.KeepHistory,
		RoomGated:     a.RoomGated,
		MessageTTL:    a.MessageTTL,
		MaxClients:    a.MaxClients,
		MaxMsgSize:    a.MaxMsgSize,
		WSInboundRate: a.WSInboundRate,
	})
}
