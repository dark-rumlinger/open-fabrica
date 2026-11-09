package probe

// WebSocket DELIVERY probe: the two-peer flow that proves an endpoint does not just
// welcome but actually DELIVERS per its model - relay broadcast, tick's two lanes,
// matchmaking pairing, authoritative state + presence, and room-gated entry. This is
// the single copy of the realtime delivery contract: the post-build validator
// (RunWebSocketSmoke, via wsDeliveryIssues) and the manage_testing test_websocket
// tool (via ProbeWSForTool) both drive endpoints through runWSDelivery, so the gate
// and the tool can never disagree about what "working" means.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// dialFunc opens one probe socket for a given query string (e.g. "?room=r&join=t").
// Each caller supplies dialing so the core stays agnostic to base URL / host / token
// shape: the validator dials ws://<host> with a Host header, the tool dials
// 127.0.0.1:<port> with the project vhost as Host.
type dialFunc func(query string) (*websocket.Conn, *http.Response, error)

// wsDeliveryFlags is the endpoint shape the delivery flow must honor.
type wsDeliveryFlags struct {
	roomGated     bool
	e2ee          bool
	authoritative bool
	hasView       bool // the reducer defined view(): omitting a peer from A's state may be intended
	tickHz        int
	matchmaking   bool
	groupSize     int
	timeout       time.Duration
	action        map[string]any // authoritative app_action (tool only); nil for the validator
}

// wsDeliveryResult is the neutral outcome both consumers format. fault is a failure
// message ("" = passed); faultKind is "contract" (a deterministic break the build can
// fix) or "transport" (a setup/flaky failure the validator must NOT fail a build on);
// faultSignature discriminates the break; warning is a soft signal (presence keying).
type wsDeliveryResult struct {
	checks         map[string]any
	fault          string
	faultKind      string
	faultSignature string
	warning        string
	clientA        string
	clientB        string
}

const (
	faultTransport = "transport"
	faultContract  = "contract"
)

// runWSDelivery drives the two-peer flow for ONE endpoint and reports what the model
// delivered. Ported from the manage_testing test_websocket tool so there is one copy.
func runWSDelivery(dial dialFunc, room, authQ string, f wsDeliveryFlags) wsDeliveryResult {
	checks := map[string]any{}
	res := wsDeliveryResult{checks: checks}
	fail := func(kind, sig, msg string, args ...any) wsDeliveryResult {
		res.fault = fmt.Sprintf(msg, args...)
		res.faultKind = kind
		res.faultSignature = sig
		return res
	}

	// Peer A: an honest first peer (no join token).
	connA, respA, err := dial("?room=" + room + authQ)
	if err != nil {
		return fail(faultTransport, "handshake", "first peer handshake failed (%s): %v", wsStatus(respA), err)
	}
	defer connA.Close(websocket.StatusNormalClosure, "probe done")
	feedA := newWSFeed(connA)
	defer feedA.stop()
	welcomeA, err := feedA.next(f.timeout)
	if err != nil || welcomeA["_type"] != "welcome" {
		return fail(faultTransport, "welcome", "first peer connected but received no welcome frame: %v", err)
	}
	checks["peer_a_welcome"] = true
	res.clientA, _ = welcomeA["_clientId"].(string)

	joinQuery := "?room=" + room + authQ
	if f.roomGated {
		tok, _ := welcomeA["_joinToken"].(string)
		if tok == "" {
			return fail(faultContract, "room_gating", "room_gated endpoint minted no _joinToken for the first peer; the invite link cannot be built")
		}
		checks["join_token_minted"] = true
		joinQuery += "&join=" + tok
	}

	// Peer B: join with the minted token (or just the room when ungated). On a
	// room_gated endpoint a valid token being rejected is a gating BUG (contract);
	// ungated, a dial failure here is transport noise the build must not fail on.
	connB, respB, err := dial(joinQuery)
	if err != nil {
		kind := faultTransport
		if f.roomGated {
			kind = faultContract
		}
		return fail(kind, "peer_b_join", "second peer could not join (resp %s): %v", wsStatus(respB), err)
	}
	defer connB.Close(websocket.StatusNormalClosure, "probe done")
	feedB := newWSFeed(connB)
	defer feedB.stop()
	welcomeB, err := feedB.next(f.timeout)
	if err != nil || welcomeB["_type"] != "welcome" {
		return fail(faultTransport, "peer_b_welcome", "second peer joined but received no welcome frame: %v", err)
	}
	checks["peer_b_joined"] = true
	res.clientB, _ = welcomeB["_clientId"].(string)

	// Non-authoritative: peer A should see B's join. Authoritative drains for a
	// 'state' frame instead (a join-drain would discard it).
	if !f.authoritative {
		if ev, _ := wsReadUntilType(feedA, "join", f.timeout); ev != nil {
			checks["peer_a_saw_join"] = true
		}
	}

	// Authoritative: the actor pushes each client a view() 'state' frame after B's
	// @join. Verify A gets one and (soft) that it references B - a roster keyed on
	// event.player.uid (undefined without requires_auth) collapses every player onto
	// one slot, so A's state never mentions B.
	if f.authoritative {
		stateJSON, serr := wsReadUntilState(feedA, f.timeout)
		if serr != nil {
			return fail(faultContract, "authoritative_state", "authoritative endpoint pushed no 'state' frame to peer A after peer B joined: %v - the reducer/actor is not broadcasting the view() state peers rely on", serr)
		}
		checks["authoritative_state_delivered"] = true
		sawB := strings.Contains(string(stateJSON), res.clientB)
		if !sawB {
			sawB = wsStateMentions(feedA, res.clientB, minDuration(f.timeout, 2*time.Second))
		}
		if len(f.action) > 0 {
			if r := checkAuthoritativeAction(feedA, feedB, connB, f.timeout, f.action, checks); r.fault != "" {
				return r
			}
		}
		if sawB {
			checks["peer_b_in_state"] = true
		} else {
			checks["peer_b_in_state"] = false
			// Warn only when the reducer has NO view(). With a view(), omitting B from
			// A's state can be intended (hidden hands), so B's absence is not evidence of
			// a bug and the warning would be noise. Stay a WARNING even with no view(): a
			// reducer may key players by seat/index and legitimately not echo client ids,
			// so the probe cannot be certain this is the uid-collapse bug.
			if !f.hasView {
				res.warning = "peer A's authoritative 'state' never referenced peer B's id (" + res.clientB + ") and the reducer defines no view(), so A receives full state. If this is a shared-presence game the reducer is likely keying players by event.player.uid (undefined on a requires_auth:false endpoint) instead of event.player.id, collapsing all players onto one slot ('2 online' for one player; peers never see each other) - key state.players on event.player.id. Ignore if the reducer tracks players by seat/index rather than by id."
			}
		}
	}

	// Matchmaking: two peers are exactly one group of 2, so the server must pair them
	// into ONE match room. A larger group_size needs more peers than this probe opens.
	if f.matchmaking {
		if f.groupSize == 2 {
			mA, errA := wsReadUntilType(feedA, "matched", f.timeout)
			mB, errB := wsReadUntilType(feedB, "matched", f.timeout)
			if errA != nil || errB != nil {
				return fail(faultContract, "matchmaking", "matchmaking lobby did not pair two waiting peers (peer A: %v, peer B: %v) - both should receive a {_type:'matched'} frame", errA, errB)
			}
			roomA, _ := mA["_room"].(string)
			roomB, _ := mB["_room"].(string)
			if roomA == "" || roomA != roomB {
				return fail(faultContract, "matchmaking", "matchmaking paired the peers into DIFFERENT rooms (%q vs %q) - they would never meet", roomA, roomB)
			}
			checks["matched_same_room"] = true
		} else {
			checks["matchmaking_skipped"] = fmt.Sprintf("group_size=%d needs more peers than this two-peer probe opens", f.groupSize)
		}
	}

	// Confirm B's message reaches A on the non-authoritative models.
	if !f.authoritative && f.tickHz > 0 {
		// Lane 1: coalesced state must arrive inside a snapshot at states[B].
		if werr := wsWriteJSON(connB, map[string]any{"x": 5, "y": 9}, f.timeout); werr != nil {
			return fail(faultTransport, "tick_snapshot", "second peer could not send a state frame on the tick endpoint: %v", werr)
		}
		if _, serr := wsReadSnapshotContaining(feedA, res.clientB, f.timeout); serr != nil {
			return fail(faultContract, "tick_snapshot", "tick endpoint (tick_hz=%d) did not deliver peer B's state to peer A in a snapshot: %v - the endpoint is not coalescing/broadcasting the snapshot peers rely on", f.tickHz, serr)
		}
		checks["snapshot_delivered"] = true
		// Lane 2: a discrete frame (net.event: chat/emote) must arrive whole, not folded away.
		if werr := wsWriteJSON(connB, map[string]any{"type": "__probe_discrete", "_discrete": true}, f.timeout); werr != nil {
			return fail(faultTransport, "tick_discrete", "second peer could not send a discrete frame on the tick endpoint: %v", werr)
		}
		ev, derr := wsReadUntilType(feedA, "__probe_discrete", f.timeout)
		if derr != nil || ev == nil {
			return fail(faultContract, "tick_discrete", "tick endpoint (tick_hz=%d) did not deliver peer B's discrete frame to peer A: %v - discrete messages (chat/emotes) are being folded into the snapshot, so net.on('chat') never fires", f.tickHz, derr)
		}
		if sender, _ := ev["_sender"].(string); sender == "" {
			return fail(faultContract, "tick_discrete", "tick endpoint delivered peer B's discrete frame but with no server-stamped _sender - identity cannot be trusted")
		}
		if _, leaked := ev["_discrete"]; leaked {
			return fail(faultContract, "tick_discrete", "the _discrete marker reached the receiving client - it is client<->server plumbing and must be stripped before broadcast")
		}
		checks["discrete_delivered"] = true
	} else if !f.authoritative {
		if werr := wsWriteJSON(connB, map[string]any{"type": "__probe_ping"}, f.timeout); werr != nil {
			return fail(faultTransport, "relay_delivery", "second peer could not send a relay frame: %v", werr)
		}
		ev, derr := wsReadUntilType(feedA, "__probe_ping", f.timeout)
		if derr != nil || ev == nil {
			return fail(faultContract, "relay_delivery", "relay endpoint did not deliver peer B's message to peer A: %v - the endpoint is not broadcasting frames between peers", derr)
		}
		if sender, _ := ev["_sender"].(string); sender == "" {
			return fail(faultContract, "relay_delivery", "relay delivered peer B's frame to peer A but with no server-stamped _sender - identity cannot be trusted")
		}
		checks["relay_delivered"] = true
	}

	// Room_gated: a third peer with no join token must be refused before upgrade (it
	// still carries any auth token, so the refusal isolates ROOM gating).
	if f.roomGated {
		connC, respC, errC := dial("?room=" + room + authQ)
		if errC != nil && wsStatusCode(respC) == http.StatusUnauthorized {
			checks["uninvited_refused_401"] = true
		} else {
			if connC != nil {
				connC.Close(websocket.StatusNormalClosure, "probe done")
			}
			return fail(faultContract, "room_gating", "a third peer with no join token was NOT refused - room gating is not enforcing entry (got %s). An uninvited peer could pull the call media", wsStatus(respC))
		}
	}

	return res
}

// checkAuthoritativeAction has peer B send an action as net.action() would and asserts
// the reducer applied it: B gets no error frame, and A receives a state with a newer rev.
func checkAuthoritativeAction(feedA, feedB *wsFeed, connB *websocket.Conn, timeout time.Duration, action map[string]any, checks map[string]any) wsDeliveryResult {
	fail := func(sig, msg string, args ...any) wsDeliveryResult {
		return wsDeliveryResult{checks: checks, fault: fmt.Sprintf(msg, args...), faultKind: faultContract, faultSignature: sig}
	}
	frame := map[string]any{}
	for k, v := range action {
		frame[k] = v
	}
	name, _ := frame["type"].(string)
	if name == "" {
		return fail("authoritative_action", "app_action needs a 'type' naming the app action, e.g. {\"type\":\"MOVE\",\"x\":1}")
	}
	delete(frame, "type")
	frame["_action"] = name
	revBefore := wsDrainStateRev(feedA, 300*time.Millisecond)
	if err := wsWriteJSON(connB, frame, timeout); err != nil {
		return fail("authoritative_action", "peer B could not send the action frame: %v", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := feedA.next(time.Until(deadline))
		if err != nil {
			break
		}
		if t, _ := m["_type"].(string); t == "error" {
			return fail("authoritative_action", "the reducer faulted on peer B's %q action: %v", name, m["_message"])
		}
		if t, _ := m["_type"].(string); t == "state" {
			if rev, ok := m["rev"].(float64); ok && rev > revBefore {
				checks["action_applied"] = true
				return wsDeliveryResult{checks: checks}
			}
		}
	}
	if m, err := wsReadUntilType(feedB, "error", 500*time.Millisecond); err == nil && m != nil {
		return fail("authoritative_action", "the reducer refused peer B's %q action: %v (ctx.reject or a fault)", name, m["_message"])
	}
	return fail("authoritative_action", "peer B's %q action produced no newer state for peer A (unknown action name? view() identical for A?)", name)
}

// --- the validator entry --------------------------------------------------------

// wsDeliveryIssues runs the delivery flow for ONE endpoint and maps the outcome to
// validator issues: a deterministic CONTRACT break is an error (drives a fix-pass),
// a transport/flaky failure is dropped (a bad socket must never block a build), and
// the presence-keying signal is a non-looping warning.
func wsDeliveryIssues(ctx context.Context, wsBase, hostHeader string, ep wsEndpoint, token string) []ValidationIssue {
	route := "/api/" + ep.path + "/ws"
	authQ := ""
	if ep.requiresAuth {
		authQ = "&token=" + token
	}
	dial := func(query string) (*websocket.Conn, *http.Response, error) {
		dialCtx, cancel := context.WithTimeout(ctx, wsSmokeReadTimeout)
		defer cancel()
		return websocket.Dial(dialCtx, wsBase+route+query, &websocket.DialOptions{Host: hostHeader})
	}
	r := runWSDelivery(dial, "of-probe-"+ep.path, authQ, wsDeliveryFlags{
		roomGated:     ep.roomGated,
		e2ee:          ep.e2ee,
		authoritative: ep.authoritative,
		hasView:       ep.hasView,
		tickHz:        ep.tickHz,
		matchmaking:   ep.matchmaking,
		groupSize:     ep.groupSize,
		timeout:       wsSmokeReadTimeout,
	})
	var out []ValidationIssue
	if r.fault != "" && r.faultKind == faultContract {
		out = append(out, ValidationIssue{
			Severity:      "error",
			Category:      "websocket",
			Page:          route,
			Deterministic: true,
			// Distinct from the welcome-smoke signatures (handshake/welcome/first_frame/
			// join_token) and the reducer smoke, so a delivery break earns its own fix-pass.
			Signature: "delivery_" + r.faultSignature,
			Message: fmt.Sprintf(
				"WebSocket endpoint %q is mounted and welcomes, but %s. Verify with manage_testing action=test_websocket endpoint_path=%s two_peer=true.",
				route, r.fault, ep.path,
			),
		})
	}
	if r.warning != "" {
		out = append(out, ValidationIssue{
			Severity:      "warning",
			Category:      "websocket",
			Page:          route,
			Deterministic: false,
			Signature:     "delivery_presence",
			Message:       fmt.Sprintf("WebSocket endpoint %q: %s", route, r.warning),
		})
	}
	return out
}

// --- the tool entry -------------------------------------------------------------

// ToolWSInput drives ProbeWSForTool (the manage_testing test_websocket action).
type ToolWSInput struct {
	Domain                         string // project vhost, sent as the Host header
	Port                           int    // 127.0.0.1:<port> the handshake actually dials
	Route                          string // canonical /api/<path>/ws
	Room                           string
	AuthQuery                      string // "&token=..." or ""
	TwoPeer                        bool
	RoomGated, E2EE, Authoritative bool
	HasView                        bool
	TickHz                         int
	Matchmaking                    bool
	GroupSize                      int
	Timeout                        time.Duration
	Action                         map[string]any
}

// ToolWSResult is the neutral outcome the tool formats into a tools.Result.
type ToolWSResult struct {
	OK       bool
	Checks   map[string]any
	Fault    string
	Status   int // HTTP status for a failed handshake (0 if none)
	Warning  string
	ClientID string
}

// ProbeWSForTool runs the single-socket welcome smoke (TwoPeer=false) or the full
// two-peer delivery flow (TwoPeer=true) and returns a neutral result. It shares
// runWSDelivery with the validator so the tool and the gate verify the same contract.
func ProbeWSForTool(in ToolWSInput) ToolWSResult {
	dial := func(query string) (*websocket.Conn, *http.Response, error) {
		dialCtx, cancel := context.WithTimeout(context.Background(), in.Timeout)
		defer cancel()
		url := fmt.Sprintf("ws://127.0.0.1:%d%s%s", in.Port, in.Route, query)
		return websocket.Dial(dialCtx, url, &websocket.DialOptions{Host: in.Domain})
	}
	if !in.TwoPeer {
		conn, resp, err := dial("?room=" + in.Room + in.AuthQuery)
		if err != nil {
			return ToolWSResult{Fault: fmt.Sprintf("WebSocket handshake failed (%s): %v", wsStatus(resp), err), Status: wsStatusCode(resp)}
		}
		defer conn.Close(websocket.StatusNormalClosure, "probe done")
		feed := newWSFeed(conn)
		defer feed.stop()
		welcome, werr := feed.next(in.Timeout)
		if werr != nil {
			return ToolWSResult{Fault: fmt.Sprintf("connected but no welcome frame arrived: %v - the endpoint upgrades but is not emitting the join/welcome contract", werr)}
		}
		if t, _ := welcome["_type"].(string); t != "welcome" {
			return ToolWSResult{Fault: fmt.Sprintf("the first frame was %q, expected a 'welcome' frame", t)}
		}
		checks := map[string]any{}
		clientID, _ := welcome["_clientId"].(string)
		if in.RoomGated {
			tok, _ := welcome["_joinToken"].(string)
			checks["join_token_present"] = tok != ""
			if tok == "" {
				return ToolWSResult{Checks: checks, ClientID: clientID, Fault: "room_gated but the first peer's welcome carried no _joinToken - the invite link cannot be built"}
			}
		}
		return ToolWSResult{OK: true, Checks: checks, ClientID: clientID}
	}
	r := runWSDelivery(dial, in.Room, in.AuthQuery, wsDeliveryFlags{
		roomGated:     in.RoomGated,
		e2ee:          in.E2EE,
		authoritative: in.Authoritative,
		hasView:       in.HasView,
		tickHz:        in.TickHz,
		matchmaking:   in.Matchmaking,
		groupSize:     in.GroupSize,
		timeout:       in.Timeout,
		action:        in.Action,
	})
	return ToolWSResult{OK: r.fault == "", Checks: r.checks, Fault: r.fault, Warning: r.warning, ClientID: r.clientA}
}

// --- socket plumbing (shared by smoke + delivery) -------------------------------

// wsFeed reads one probe connection on its own goroutine and hands frames over a
// channel, so every timeout bounds a channel wait, never a conn.Read. coder/websocket
// CLOSES a connection whose Read context expires, so a bounded read that ran out its
// window (a drain, an optional wait) would kill the socket and fail every later check.
type wsFeed struct {
	frames chan wsFrame
	done   chan struct{}
	once   sync.Once
}

type wsFrame struct {
	m   map[string]any
	err error
}

func newWSFeed(conn *websocket.Conn) *wsFeed {
	f := &wsFeed{frames: make(chan wsFrame, 64), done: make(chan struct{})}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			fr := wsFrame{err: err}
			if err == nil {
				if jerr := json.Unmarshal(data, &fr.m); jerr != nil {
					fr = wsFrame{err: fmt.Errorf("non-JSON frame: %s", string(data))}
				}
			}
			select {
			case f.frames <- fr:
			case <-f.done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return f
}

func (f *wsFeed) stop() { f.once.Do(func() { close(f.done) }) }

func (f *wsFeed) next(timeout time.Duration) (map[string]any, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case fr := <-f.frames:
		if fr.err != nil && fr.m == nil {
			if !strings.HasPrefix(fr.err.Error(), "non-JSON frame") {
				select {
				case f.frames <- fr:
				default:
				}
			}
		}
		return fr.m, fr.err
	case <-t.C:
		return nil, fmt.Errorf("no frame within %s", timeout)
	}
}

// wsReadUntilType drains frames until one of the given _type (or app type) arrives.
func wsReadUntilType(feed *wsFeed, wantType string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := feed.next(time.Until(deadline))
		if err != nil {
			return nil, err
		}
		if t, _ := m["_type"].(string); t == wantType {
			return m, nil
		}
		if t, _ := m["type"].(string); t == wantType {
			return m, nil
		}
	}
	return nil, fmt.Errorf("no %q frame within timeout", wantType)
}

// wsReadUntilState drains until an authoritative {_type:'state'} frame arrives,
// returning its state payload re-encoded as JSON.
func wsReadUntilState(feed *wsFeed, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := feed.next(time.Until(deadline))
		if err != nil {
			return nil, err
		}
		if t, _ := m["_type"].(string); t == "state" {
			b, merr := json.Marshal(m["state"])
			if merr != nil {
				return nil, fmt.Errorf("state frame carried an unmarshalable payload: %w", merr)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("no authoritative 'state' frame within timeout")
}

// wsStateMentions drains 'state' frames for up to window and reports whether any carries clientID.
func wsStateMentions(feed *wsFeed, clientID string, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		b, err := wsReadUntilState(feed, time.Until(deadline))
		if err != nil {
			return false
		}
		if strings.Contains(string(b), clientID) {
			return true
		}
	}
	return false
}

// wsDrainStateRev reads frames for up to d and returns the highest state rev seen (-1 when none).
func wsDrainStateRev(feed *wsFeed, d time.Duration) float64 {
	best := -1.0
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m, err := feed.next(time.Until(deadline))
		if err != nil {
			break
		}
		if t, _ := m["_type"].(string); t == "state" {
			if rev, ok := m["rev"].(float64); ok && rev > best {
				best = rev
			}
		}
	}
	return best
}

// wsReadSnapshotContaining drains until a tick 'snapshot' frame whose states map carries clientID.
func wsReadSnapshotContaining(feed *wsFeed, clientID string, timeout time.Duration) (map[string]any, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := feed.next(time.Until(deadline))
		if err != nil {
			return nil, err
		}
		if snapshotContainsClient(m, clientID) {
			return m, nil
		}
	}
	return nil, fmt.Errorf("no snapshot carrying client %q within timeout", clientID)
}

// snapshotContainsClient reports whether frame is a tick 'snapshot' whose states map
// holds an entry for clientID. Pure (no I/O) so tick delivery is unit-testable.
func snapshotContainsClient(frame map[string]any, clientID string) bool {
	if t, _ := frame["_type"].(string); t != "snapshot" {
		return false
	}
	states, ok := frame["states"].(map[string]any)
	if !ok {
		return false
	}
	_, present := states[clientID]
	return present
}

func wsWriteJSON(conn *websocket.Conn, v any, timeout time.Duration) error {
	writeCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(writeCtx, websocket.MessageText, b)
}

func wsStatusCode(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func wsStatus(resp *http.Response) string {
	if resp == nil {
		return "no HTTP response"
	}
	return resp.Status
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
