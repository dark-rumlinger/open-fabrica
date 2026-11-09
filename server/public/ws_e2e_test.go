package public

// End-to-end over real sockets: the audit fixes exercised through apiWebSocket itself
// (the read pump, the hub, the room actor) rather than one layer at a time.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"openfabrica/db"
)

type wsE2E struct {
	t   *testing.T
	srv *httptest.Server
}

func newWSE2E(t *testing.T, endpoints ...string) *wsE2E {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	for _, q := range endpoints {
		if _, err := pdb.Writer().Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	h := &Handler{deps: &Deps{WSHub: NewWSHub()}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.apiWebSocket(w, r, 1, pdb)
	}))
	t.Cleanup(srv.Close)
	return &wsE2E{t: t, srv: srv}
}

func (e *wsE2E) dial(path, query string) *websocket.Conn {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/" + path + "/ws?" + query
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		e.t.Fatalf("dial %s: %v", url, err)
	}
	e.t.Cleanup(func() { c.CloseNow() })
	return c
}

// until reads frames until pred matches one (returning it) or the timeout passes.
func until(t *testing.T, c *websocket.Conn, pred func(map[string]any) bool) (map[string]any, error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if json.Unmarshal(data, &m) == nil && pred(m) {
			return m, nil
		}
	}
	return nil, errors.New("timeout")
}

func typed(want string) func(map[string]any) bool {
	return func(m map[string]any) bool { t, _ := m["_type"].(string); return t == want }
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

const e2eReducer = `function reduce(state, event, ctx) {
  if (event.type === '@init') return { settled: false, roster: 0, pokes: 0 };
  if (event.type === '@timer' && event.name === 'settle') { state.settled = true; return state; }
  if (event.type === '@join' || event.type === '@leave') { state.roster = ctx.players.length; return state; }
  if (event.type === 'poke') { state.pokes++; state.roster = ctx.players.length; return state; }
  return state;
}`

func TestE2E_AuthoritativeForgeryReconnectAndSupersede(t *testing.T) {
	e := newWSE2E(t, `INSERT INTO of_ws_endpoints (path, model, authoritative, reducer) VALUES ('game', 'authoritative', 1, '`+strings.ReplaceAll(e2eReducer, "'", "''")+`')`)

	a := e.dial("game", "room=r&cid=c-alice-secret")
	if _, err := until(t, a, typed("state")); err != nil {
		t.Fatalf("A got no initial state: %v", err)
	}
	b := e.dial("game", "room=r&cid=c-bob-secret-1")
	st, err := until(t, a, func(m map[string]any) bool {
		s, _ := m["state"].(map[string]any)
		return m["_type"] == "state" && s != nil && s["roster"] == float64(2)
	})
	if err != nil {
		t.Fatalf("A never saw a 2-player roster: %v", err)
	}
	_ = st

	// A forged lifecycle event is refused to the sender and changes nothing.
	send(t, b, map[string]any{"_action": "@timer", "name": "settle"})
	if m, err := until(t, b, typed("error")); err != nil || !strings.Contains(m["_message"].(string), "server-only") {
		t.Fatalf("forged @timer was not refused: %v %v", m, err)
	}

	// B reconnects with the same cid: the old socket is closed with 4001 and the
	// roster stays at 2 (no duplicate entry).
	b2 := e.dial("game", "room=r&cid=c-bob-secret-1")
	_, err = until(t, b, func(map[string]any) bool { return false })
	if websocket.CloseStatus(err) != wsCloseSuperseded {
		t.Fatalf("superseded socket closed with %v (status %d), want %d", err, websocket.CloseStatus(err), wsCloseSuperseded)
	}
	send(t, b2, map[string]any{"_action": "poke"})
	m, err := until(t, a, func(m map[string]any) bool {
		s, _ := m["state"].(map[string]any)
		return m["_type"] == "state" && s != nil && s["pokes"] == float64(1)
	})
	if err != nil {
		t.Fatalf("A never saw the poke: %v", err)
	}
	s := m["state"].(map[string]any)
	if s["roster"] != float64(2) {
		t.Fatalf("roster after reconnect = %v, want 2 (duplicate player)", s["roster"])
	}
	if s["settled"] == true {
		t.Fatal("the forged @timer settled the room")
	}
}

func TestE2E_MatchmakingPairsIntoCappedRandomRoom(t *testing.T) {
	e := newWSE2E(t, `INSERT INTO of_ws_endpoints (path, model, matchmaking, group_size, room_column) VALUES ('lobby', 'matchmaking', 1, 2, 'room')`)
	a := e.dial("lobby", "room=lobby&cid=c-alice-secret")
	b := e.dial("lobby", "room=lobby&cid=c-bob-secret-1")
	ma, errA := until(t, a, typed("matched"))
	mb, errB := until(t, b, typed("matched"))
	if errA != nil || errB != nil {
		t.Fatalf("peers not matched: %v / %v", errA, errB)
	}
	room, _ := ma["_room"].(string)
	if room == "" || room != mb["_room"] || room == "m-1" {
		t.Fatalf("matched rooms %v / %v - want one shared random id", ma["_room"], mb["_room"])
	}
	send(t, a, map[string]any{"room": room})
	send(t, b, map[string]any{"room": room})
	if _, err := until(t, a, typed("welcome")); err != nil {
		t.Fatalf("A did not switch into the match: %v", err)
	}
	if _, err := until(t, b, typed("welcome")); err != nil {
		t.Fatalf("B did not switch into the match: %v", err)
	}

	intruder := e.dial("lobby", "room="+room+"&cid=c-mallory-secret")
	w, err := until(t, intruder, typed("welcome"))
	if err != nil {
		t.Fatalf("intruder got no welcome: %v", err)
	}
	if w["_role"] != "spectator" {
		t.Fatalf("a third peer joined the match as %v, want spectator", w["_role"])
	}
	if _, err := until(t, intruder, typed("matched")); err == nil {
		t.Fatal("pairing ran inside a match room")
	}
}

func TestE2E_ActionOnRelayIsRefusedVisibly(t *testing.T) {
	e := newWSE2E(t, `INSERT INTO of_ws_endpoints (path, model) VALUES ('chat', 'relay')`)
	a := e.dial("chat", "room=r&cid=c-alice-secret")
	b := e.dial("chat", "room=r&cid=c-bob-secret-1")
	if _, err := until(t, a, typed("join")); err != nil {
		t.Fatalf("A never saw B join: %v", err)
	}
	send(t, b, map[string]any{"_action": "move", "x": 1})
	if m, err := until(t, b, typed("error")); err != nil || !strings.Contains(m["_message"].(string), "authoritative") {
		t.Fatalf("net.action on a relay was not refused: %v %v", m, err)
	}
	send(t, b, map[string]any{"type": "chat", "text": "hi"})
	got, err := until(t, a, func(m map[string]any) bool { return m["type"] == "chat" || m["_action"] != nil })
	if err != nil || got["_action"] != nil {
		t.Fatalf("A received %v (err %v) - the refused action leaked or chat broke", got, err)
	}
}
