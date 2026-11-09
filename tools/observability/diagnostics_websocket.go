package observability

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"openfabrica/internal/roomactor"
	"openfabrica/probe"
	"openfabrica/tools"
)

// DiagTestWebSocket actually drives a WebSocket endpoint: it completes the
// handshake and reads the server's welcome frame, which is the only check that
// proves a realtime endpoint works. test_url can only confirm the route is
// mounted (a plain GET yields 426); it cannot tell a healthy relay from one that
// upgrades and then never speaks, which is exactly how a broken call ships green.
//
// Default mode is a single-socket smoke: connect, assert a welcome frame
// arrives. The opt-in two_peer mode exercises the room_gated capability flow
// plus actual delivery between two peers. Both modes run through the SAME
// delivery contract the post-build validator uses (probe.ProbeWSForTool /
// probe.RunWebSocketSmoke share probe/runtime_ws_delivery.go), so the tool and
// the automatic gate can never disagree about what "working" means.
type DiagTestWebSocket struct {
	EndpointPath string `arg:"endpoint_path,required" desc:"WebSocket endpoint path, e.g. 'call' (also accepts '/api/call/ws')."`
	Room         string `arg:"room"                   desc:"Room to join. Defaults to a unique throwaway probe room."`
	TwoPeer      bool   `arg:"two_peer"               desc:"Run the full two-peer flow (room_gated mint/join/refuse + one relayed message) instead of the default single-socket smoke."`
	AuthToken    string `arg:"auth_token"             desc:"JWT for a requires_auth endpoint (every probe peer connects with it). Mint one with action=test_auth_flow - its result carries 'token'. Without it an auth-gated handshake is refused 401 before the upgrade."`
	TimeoutMs    int    `arg:"timeout_ms"             desc:"Per-frame read timeout." default:"6000"`
	// Action is sent by peer B through the same _action frame net.action() builds, so the
	// probe exercises the reducer's app-action path (not just @join).
	Action map[string]any `arg:"app_action" desc:"Authoritative + two_peer: an action peer B sends as net.action() would, e.g. {\"type\":\"MOVE\",\"x\":1}; asserts peer A gets a newer state."`
}

func (a *DiagTestWebSocket) ActionName() string { return "test_websocket" }
func (a *DiagTestWebSocket) Doc() string {
	return "Open a real WebSocket to a relay, tick, authoritative, matchmaking or signaling endpoint: handshake plus the welcome frame. two_peer=true also checks the room_gated token flow and that the endpoint's model delivers between two peers (app_action drives one authoritative action). A requires_auth endpoint needs auth_token from test_auth_flow. Prefer this over test_url for chat, call and multiplayer endpoints."
}
func (a *DiagTestWebSocket) ReadOnly() bool { return true }
func (a *DiagTestWebSocket) Example() any {
	return &DiagTestWebSocket{EndpointPath: "call", TwoPeer: true}
}

var wsProbeRoomSeq uint64

func (a *DiagTestWebSocket) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}
	wsPath := normalizeWSPath(a.EndpointPath)

	// Read the endpoint's flags so the result can interpret the welcome frame and
	// drive the right delivery lane (room_gated mints a token; e2ee shapes
	// handling; tick_hz>0 verifies the snapshot path; authoritative expects state).
	roomGated, e2ee, requiresAuth, authoritative, hasView, tickHz, exists := wsEndpointFlags(ctx, wsPath)
	if !exists {
		return tools.Fail("no WebSocket endpoint is mounted at %s - create one with a realtime tool (manage_relay/manage_tick/manage_authoritative/manage_matchmaking/manage_signaling), or check the path", wsPath), nil
	}
	// Fail fast, and teach, when the endpoint is auth-gated and the probe holds no
	// token: the handshake is refused 401 before the upgrade, ALWAYS, so dialing
	// anyway reads as a broken relay when the relay is fine.
	if requiresAuth && a.AuthToken == "" {
		return tools.Fail("%s has requires_auth=true, so an unauthenticated handshake is refused 401 before the upgrade - that is the auth gate working, not a broken relay. Mint a JWT first (action=test_auth_flow; its result carries `token`) and re-run this probe with auth_token=<that token>.", wsPath), nil
	}

	timeout := time.Duration(a.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	room := a.Room
	if room == "" {
		n := atomic.AddUint64(&wsProbeRoomSeq, 1)
		room = fmt.Sprintf("of-probe-%d", n)
	}
	authQ := ""
	if a.AuthToken != "" {
		authQ = "&token=" + url.QueryEscape(a.AuthToken)
	}

	in := probe.ToolWSInput{
		Domain:        domain,
		Port:          port,
		Route:         wsPath,
		Room:          room,
		AuthQuery:     authQ,
		TwoPeer:       a.TwoPeer,
		RoomGated:     roomGated,
		E2EE:          e2ee,
		Authoritative: authoritative,
		HasView:       hasView,
		TickHz:        tickHz,
		Timeout:       timeout,
		Action:        a.Action,
	}
	if a.TwoPeer {
		in.Matchmaking, in.GroupSize = wsEndpointMatchmaking(ctx, wsPath)
		return a.renderTwoPeer(wsPath, room, e2ee, roomGated, authoritative, tickHz, probe.ProbeWSForTool(in)), nil
	}
	return a.renderSmoke(wsPath, room, e2ee, roomGated, probe.ProbeWSForTool(in)), nil
}

// renderSmoke formats the single-socket welcome result.
func (a *DiagTestWebSocket) renderSmoke(wsPath, room string, e2ee, roomGated bool, r probe.ToolWSResult) *tools.Result {
	if !r.OK {
		data := map[string]any{"endpoint": wsPath}
		if r.Status != 0 {
			data["status"] = r.Status
		}
		if len(r.Checks) > 0 {
			data["checks"] = r.Checks
		}
		return &tools.Result{Success: false, Data: data, Error: fmt.Sprintf("%s: %s", wsPath, r.Fault)}
	}
	note := "Handshake completed and the relay emitted a welcome frame - the endpoint is live and speaking the relay contract."
	if roomGated {
		note = "Handshake completed; first peer minted a _joinToken (room_gated working). Run two_peer=true to verify the second peer can join with it."
	}
	return tools.Ok(map[string]any{
		"endpoint":   wsPath,
		"passed":     true,
		"served":     "websocket",
		"room":       room,
		"client_id":  r.ClientID,
		"e2ee":       e2ee,
		"room_gated": roomGated,
		"checks":     r.Checks,
		"note":       note,
	})
}

// renderTwoPeer formats the two-peer delivery result.
func (a *DiagTestWebSocket) renderTwoPeer(wsPath, room string, e2ee, roomGated, authoritative bool, tickHz int, r probe.ToolWSResult) *tools.Result {
	if !r.OK {
		return &tools.Result{
			Success: false,
			Data:    map[string]any{"endpoint": wsPath, "room": room, "checks": r.Checks},
			Error:   fmt.Sprintf("%s: %s", wsPath, r.Fault),
		}
	}
	delivery := "the relay delivered a message between them"
	switch {
	case authoritative:
		delivery = "the authoritative reducer pushed peer B's presence to peer A in a view() state frame"
		if w, _ := r.Checks["peer_b_in_state"].(bool); !w {
			delivery += " (WARNING: peer A's state did not reference peer B - the reducer may key its roster on event.player.uid instead of event.player.id; see checks.presence or the warning)"
		}
	case tickHz > 0:
		delivery = "the tick endpoint delivered BOTH lanes: peer B's continuous state reached peer A coalesced in a snapshot, and peer B's discrete frame (net.event) reached peer A whole as its own frame"
	}
	data := map[string]any{
		"endpoint":      wsPath,
		"passed":        true,
		"room":          room,
		"e2ee":          e2ee,
		"tick_hz":       tickHz,
		"authoritative": authoritative,
		"room_gated":    roomGated,
		"checks":        r.Checks,
		"note":          "Two-peer flow verified: both peers joined, " + delivery + ternary(roomGated, ", and an uninvited peer was refused.", "."),
	}
	if r.Warning != "" {
		data["presence_warning"] = r.Warning
	}
	return tools.Ok(data)
}

// --- helpers ----------------------------------------------------------------

// normalizeWSPath accepts 'call', 'call/ws', '/api/call/ws', or '/call' and
// returns the canonical '/api/<path>/ws' route.
func normalizeWSPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "/")
	p = strings.TrimPrefix(p, "api/")
	p = strings.TrimSuffix(p, "/ws")
	p = strings.Trim(p, "/")
	return "/api/" + p + "/ws"
}

// wsEndpointFlags reads room_gated / e2ee / requires_auth / authoritative /
// hasView / tick_hz for the endpoint behind a '/api/<p>/ws' route, and whether
// such a row exists. authoritative is true when a reducer is attached; hasView is
// true when that reducer defines a view() (and defaults true when it will not
// compile here, so an unanalyzable endpoint is never gated as a presence collapse).
func wsEndpointFlags(ctx *tools.ToolContext, wsPath string) (roomGated, e2ee, requiresAuth, authoritative, hasView bool, tickHz int, exists bool) {
	path := strings.TrimSuffix(strings.TrimPrefix(wsPath, "/api/"), "/ws")
	var rg, e, ra sql.NullBool
	var th sql.NullInt64
	var reducer string
	err := ctx.DB.QueryRow(
		"SELECT COALESCE(room_gated,0), COALESCE(e2ee,0), COALESCE(requires_auth,0), COALESCE(reducer,''), COALESCE(tick_hz,0) FROM of_ws_endpoints WHERE path = ?", path,
	).Scan(&rg, &e, &ra, &reducer, &th)
	if err != nil {
		return false, false, false, false, false, 0, false
	}
	authoritative = strings.TrimSpace(reducer) != ""
	hasView = true
	if authoritative {
		if red, cerr := roomactor.Compile(reducer); cerr == nil {
			hasView = red.HasView()
		}
	}
	return rg.Bool, e.Bool, ra.Bool, authoritative, hasView, int(th.Int64), true
}

// wsEndpointMatchmaking reads whether the endpoint is a matchmaking lobby and its group size.
func wsEndpointMatchmaking(ctx *tools.ToolContext, wsPath string) (bool, int) {
	path := strings.TrimSuffix(strings.TrimPrefix(wsPath, "/api/"), "/ws")
	var mm bool
	var gs int
	if err := ctx.DB.QueryRow("SELECT COALESCE(matchmaking,0), COALESCE(group_size,2) FROM of_ws_endpoints WHERE path = ?", path).Scan(&mm, &gs); err != nil {
		return false, 0
	}
	return mm, gs
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
