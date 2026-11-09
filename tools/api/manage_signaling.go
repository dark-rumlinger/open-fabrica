package api

import (
	"openfabrica/internal/wsendpoint"
	"openfabrica/tools"
)

// manage_signaling is the WebRTC SIGNALING realtime model: the relay forwards
// SDP/ICE frames between peers immediately (tick coalescing disabled, a larger
// frame cap) so they can open a peer-to-peer RTCPeerConnection; media never
// touches the server. It is a distinct model from relay/tick/authoritative and
// pairs with the webrtc_signaling guide's client contract (Perfect Negotiation,
// ICE buffering, TURN). Shares the of_ws_endpoints runtime (model=signaling).

func init() {
	tools.Define(tools.Tool{
		Name:               "manage_signaling",
		RealtimeModel:      "webrtc",
		Category:           "api",
		FullResultInEvents: true,
		MaxResult:          16000,
		OneLiner:           "WebRTC signaling relay for video/voice calls: forwards SDP/ICE over a WebSocket so peers open peer-to-peer audio/video/data.",
		WhenToUse: `Use ONLY for WebRTC: peer-to-peer audio/video (getUserMedia +
RTCPeerConnection) or a data channel. The relay forwards SDP/ICE immediately
(no tick) and never sees the media. You still write the RTCPeerConnection
client - follow the webrtc_signaling guide (Perfect Negotiation, ICE buffering
before setRemoteDescription, TURN not STUN-only, no simple-peer/peerjs
wrappers). For private calls add room_gated (invite-only) and e2ee. For plain
text chat/state (no media) use manage_relay/manage_tick instead - none of the
WebRTC machinery is needed there. Read/delete with manage_realtime.`,
		Actions: []tools.Action{
			&SignalingCreate{},
			&SignalingUpdate{},
		},
		Summarize: EpSummarize,
	})
}

// --- create -----------------------------------------------------------------

type SignalingCreate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Literal path suffix (no /api/, no leading slash, no :param)."`
	RequiresAuth bool   `arg:"requires_auth"          desc:"Connections must include a valid token (connect() auto-carries it)."`
	RoomGated    bool   `arg:"room_gated"             desc:"Invite-only calls: gate room ENTRY on a per-room ?join=<token> (first peer mints it, returned in welcome as _joinToken). Defaults ON when e2ee=true. Use so knowing the room name is not enough to pull peer-to-peer media."`
	E2EE         bool   `arg:"e2ee"                   desc:"Enforce end-to-end encryption on the accompanying chat channel and emit js/e2ee.js. Signaling frames (offer/answer/ice) still pass in the clear; media is already peer-to-peer. Defaults room_gated ON."`
	RoomColumn   string `arg:"room_column"            desc:"Column name used for room scoping (one call room per key)."`
	MaxClients   int    `arg:"max_clients"            desc:"Cap on peers per call room (0 = unlimited); 2 for a 1:1 call. Overflow joiners connect as read-only spectators (welcome._role='spectator'), enforced atomically on join - a client-side cap loses a simultaneous-join race."`
	MaxMsgSize   int    `arg:"max_msg_size"           desc:"Hard cap on single inbound frame bytes, max 1048576 (1 MiB). Signaling defaults to 65536 (SDP with many ICE candidates runs 10-40KB)."`
}

func (a *SignalingCreate) Doc() string {
	return "Create a WebRTC signaling endpoint (immediate SDP/ICE relay; tick_hz is fixed at 0)."
}
func (a *SignalingCreate) Example() any {
	return &SignalingCreate{EndpointPath: "call", RoomGated: true, MaxClients: 2}
}
func (a *SignalingCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return epCreateWebSocket(ctx, wsEndpointConfig{
		Model:        wsendpoint.ModelSignaling,
		Path:         a.EndpointPath,
		RequiresAuth: &a.RequiresAuth,
		Mode:         "signaling",
		RoomGated:    a.RoomGated,
		E2EE:         a.E2EE,
		RoomColumn:   a.RoomColumn,
		MaxClients:   &a.MaxClients,
		MaxMsgSize:   a.MaxMsgSize,
	})
}

// --- update -----------------------------------------------------------------

type SignalingUpdate struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"Signaling endpoint path to update."`
	RequiresAuth *bool  `arg:"requires_auth"          desc:"Toggle the auth requirement on (true) or off (false). Omit to leave unchanged."`
	RoomGated    bool   `arg:"room_gated"             desc:"Turn on the per-room access-token entry gate. Only flips on."`
	E2EE         bool   `arg:"e2ee"                   desc:"Turn on end-to-end encryption (see create). Only flips on."`
	RoomColumn   string `arg:"room_column"            desc:"Replace the room-scope column."`
	MaxClients   *int   `arg:"max_clients"            desc:"Replace the per-room peer cap; 0 lifts the cap (unlimited). Omit to leave unchanged."`
	MaxMsgSize   int    `arg:"max_msg_size"           desc:"Replace single-frame byte cap, max 1048576 (1 MiB)."`
}

func (a *SignalingUpdate) Doc() string { return "Update a WebRTC signaling endpoint's options." }
func (a *SignalingUpdate) Example() any {
	return &SignalingUpdate{EndpointPath: "call", E2EE: true}
}
func (a *SignalingUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	// No Mode here: create already fixed tick_hz=0 and the SDP frame cap, and the model
	// check guarantees this row is a signaling endpoint. Passing Mode on every update
	// rewrote tick_hz and reset a custom max_msg_size to 64KB on each call.
	return epUpdateWebSocket(ctx, wsEndpointConfig{
		Model:        wsendpoint.ModelSignaling,
		Path:         a.EndpointPath,
		RequiresAuth: a.RequiresAuth,
		RoomGated:    a.RoomGated,
		E2EE:         a.E2EE,
		RoomColumn:   a.RoomColumn,
		MaxClients:   a.MaxClients,
		MaxMsgSize:   a.MaxMsgSize,
	})
}
