package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// TestSnapshotContainsClient pins the tick-delivery decision the two-peer probe
// uses on a tick_hz>0 endpoint: peer B's state reaches peer A ONLY coalesced
// inside a `snapshot` frame's `states[B]`, never as a discrete relayed frame.
func TestSnapshotContainsClient(t *testing.T) {
	snapWithB := map[string]any{
		"_type":  "snapshot",
		"_room":  "r",
		"states": map[string]any{"cid-b": map[string]any{"x": 5.0}},
	}
	if !snapshotContainsClient(snapWithB, "cid-b") {
		t.Error("snapshot carrying cid-b in states should count as delivery")
	}
	if snapshotContainsClient(snapWithB, "cid-a") {
		t.Error("snapshot NOT carrying cid-a should not count as delivery")
	}
	discrete := map[string]any{"type": "chat", "text": "hi", "_sender": "cid-b"}
	if snapshotContainsClient(discrete, "cid-b") {
		t.Error("a discrete type-frame must not be treated as a snapshot delivery")
	}
	if snapshotContainsClient(map[string]any{"_type": "snapshot"}, "cid-b") {
		t.Error("a snapshot with no states map should not count")
	}
	if snapshotContainsClient(map[string]any{"_type": "welcome", "states": map[string]any{"cid-b": 1}}, "cid-b") {
		t.Error("a non-snapshot frame must not count even if it has a states field")
	}
}

// fakeRelayHub is a minimal relay: it welcomes each peer, announces joins to the
// others, and broadcasts every received frame to the OTHER peers in the room with
// a server-stamped _sender (and the _discrete marker stripped). With gated=true it
// mints a _joinToken for the first peer and refuses a later peer that has no
// ?join=<token>, so the two-peer probe's full gating + delivery path can run
// against an in-process server with no real OpenFabrica runtime.
type fakeRelayHub struct {
	gated bool
	mu    sync.Mutex
	rooms map[string][]*websocket.Conn
	seq   int
}

func newFakeRelayHub(t *testing.T, gated bool) (*httptest.Server, string) {
	t.Helper()
	h := &fakeRelayHub{gated: gated, rooms: map[string][]*websocket.Conn{}}
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	return srv, strings.Replace(srv.URL, "http://", "ws://", 1)
}

func (h *fakeRelayHub) serve(w http.ResponseWriter, r *http.Request) {
	room := r.URL.Query().Get("room")
	join := r.URL.Query().Get("join")

	h.mu.Lock()
	first := len(h.rooms[room]) == 0
	if h.gated && !first && join != "tok-"+room {
		h.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.mu.Unlock()

	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()

	h.mu.Lock()
	h.seq++
	id := "c" + itoa(h.seq)
	h.rooms[room] = append(h.rooms[room], c)
	others := peersExcept(h.rooms[room], c)
	h.mu.Unlock()

	welcome := map[string]any{"_type": "welcome", "_clientId": id, "_room": room}
	if h.gated && first {
		welcome["_joinToken"] = "tok-" + room
	}
	_ = wsjson.Write(ctx, c, welcome)
	for _, o := range others {
		_ = wsjson.Write(ctx, o, map[string]any{"_type": "join", "_sender": id, "_room": room})
	}

	for {
		var frame map[string]any
		if err := wsjson.Read(ctx, c, &frame); err != nil {
			return
		}
		delete(frame, "_discrete")
		frame["_sender"] = id
		h.mu.Lock()
		dst := peersExcept(h.rooms[room], c)
		h.mu.Unlock()
		for _, o := range dst {
			_ = wsjson.Write(ctx, o, frame)
		}
	}
}

func peersExcept(conns []*websocket.Conn, self *websocket.Conn) []*websocket.Conn {
	var out []*websocket.Conn
	for _, c := range conns {
		if c != self {
			out = append(out, c)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// dialTo builds a dialFunc that dials wsBase+route+query against the fake hub.
func dialTo(wsBase string) dialFunc {
	return func(query string) (*websocket.Conn, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return websocket.Dial(ctx, wsBase+query, nil)
	}
}

// TestRunWSDelivery_RelayBroadcasts: an ungated relay must deliver peer B's frame
// to peer A with a server-stamped _sender - the delivery check the welcome smoke
// alone cannot make.
func TestRunWSDelivery_RelayBroadcasts(t *testing.T) {
	_, wsBase := newFakeRelayHub(t, false)
	r := runWSDelivery(dialTo(wsBase), "lobby", "", wsDeliveryFlags{timeout: 3 * time.Second})
	if r.fault != "" {
		t.Fatalf("healthy relay reported a fault: %s (checks %v)", r.fault, r.checks)
	}
	if ok, _ := r.checks["relay_delivered"].(bool); !ok {
		t.Errorf("expected relay_delivered=true, got checks %v", r.checks)
	}
	if ok, _ := r.checks["peer_b_joined"].(bool); !ok {
		t.Errorf("expected peer_b_joined=true, got checks %v", r.checks)
	}
}

// TestRunWSDelivery_RoomGated: the first peer mints a token, the second joins with
// it, and a third peer with no token is refused 401.
func TestRunWSDelivery_RoomGated(t *testing.T) {
	_, wsBase := newFakeRelayHub(t, true)
	r := runWSDelivery(dialTo(wsBase), "vip", "", wsDeliveryFlags{roomGated: true, timeout: 3 * time.Second})
	if r.fault != "" {
		t.Fatalf("healthy gated relay reported a fault: %s (checks %v)", r.fault, r.checks)
	}
	for _, k := range []string{"join_token_minted", "peer_b_joined", "relay_delivered", "uninvited_refused_401"} {
		if ok, _ := r.checks[k].(bool); !ok {
			t.Errorf("expected %s=true, got checks %v", k, r.checks)
		}
	}
}

// TestRunWSDelivery_SilentRelayFaultsContract: a server that welcomes both peers
// but never broadcasts is a CONTRACT break (not transport), so the validator fails
// the build on it.
func TestRunWSDelivery_SilentRelayFaultsContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = wsjson.Write(r.Context(), c, map[string]any{"_type": "welcome", "_clientId": "x"})
		// Welcome, then never relay anything.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	wsBase := strings.Replace(srv.URL, "http://", "ws://", 1)
	r := runWSDelivery(dialTo(wsBase), "lobby", "", wsDeliveryFlags{timeout: 1500 * time.Millisecond})
	if r.fault == "" {
		t.Fatal("a relay that welcomes but never delivers must fault")
	}
	if r.faultKind != faultContract {
		t.Errorf("a non-delivering relay is a contract break, got kind %q (sig %q)", r.faultKind, r.faultSignature)
	}
}
