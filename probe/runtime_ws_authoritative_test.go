package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeAuthoritativeServer is a minimal authoritative room: a welcome per client, a
// state push to everyone when a peer joins, and a new state (rev+1) after every
// _action. Returns the ws:// base URL.
func fakeAuthoritativeServer(t *testing.T) string {
	t.Helper()
	var (
		mu    sync.Mutex
		conns []*websocket.Conn
		rev   int
	)
	push := func() {
		mu.Lock()
		rev++
		frame, _ := json.Marshal(map[string]any{"_type": "state", "rev": rev, "state": map[string]any{"online": len(conns)}})
		targets := append([]*websocket.Conn(nil), conns...)
		mu.Unlock()
		for _, c := range targets {
			_ = c.Write(context.Background(), websocket.MessageText, frame)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		conns = append(conns, c)
		id := "c" + itoa(len(conns))
		mu.Unlock()
		welcome, _ := json.Marshal(map[string]any{"_type": "welcome", "_clientId": id})
		_ = c.Write(context.Background(), websocket.MessageText, welcome)
		push() // @join
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) == nil && m["_action"] != nil {
				push()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http://", "ws://", 1)
}

// TestRunWSDelivery_AuthoritativeActionSeesNewerState: the app_action path drains
// peer A with reads whose timeout, in coder/websocket, would CLOSE the connection -
// so peer B's action would never be seen and every reducer reads "no newer state".
// A room that does apply the action must report action_applied.
func TestRunWSDelivery_AuthoritativeActionSeesNewerState(t *testing.T) {
	wsBase := fakeAuthoritativeServer(t)
	r := runWSDelivery(dialTo(wsBase), "r1", "", wsDeliveryFlags{
		authoritative: true,
		action:        map[string]any{"type": "PING"},
		timeout:       3 * time.Second,
	})
	if r.fault != "" {
		t.Fatalf("an applied action must pass the two-peer delivery, got fault %q (checks %v)", r.fault, r.checks)
	}
	if r.checks["action_applied"] != true {
		t.Fatalf("want checks.action_applied, got %v", r.checks)
	}
}

// TestRunWSDelivery_PresenceWarnGatedByView: the fake room pushes state {online:N}
// that never carries a client id, so peer B is "absent" from peer A's state. With no
// view() that earns the uid-collapse warning; with a view() present (peer-hiding may be
// intended) the warning is suppressed. Either way it is a warning, never a hard fault.
func TestRunWSDelivery_PresenceWarnGatedByView(t *testing.T) {
	wsBase := fakeAuthoritativeServer(t)
	noView := runWSDelivery(dialTo(wsBase), "r1", "", wsDeliveryFlags{
		authoritative: true, hasView: false, timeout: 3 * time.Second,
	})
	if noView.fault != "" {
		t.Fatalf("a missing peer must not hard-fail the build, got fault %q", noView.fault)
	}
	if noView.warning == "" {
		t.Errorf("a no-view authoritative room that omits peer B must warn, got none (checks %v)", noView.checks)
	}

	withView := runWSDelivery(dialTo(wsBase), "r2", "", wsDeliveryFlags{
		authoritative: true, hasView: true, timeout: 3 * time.Second,
	})
	if withView.warning != "" {
		t.Errorf("a view() endpoint may hide peers on purpose, so no presence warning is expected, got %q", withView.warning)
	}
}

// TestWSFeedTimeoutKeepsConnectionOpen: a wait that runs out its window must NOT
// close the connection - the frame that arrives afterwards is still readable.
func TestWSFeedTimeoutKeepsConnectionOpen(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-release
		_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"_type":"late"}`))
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	wsBase := strings.Replace(srv.URL, "http://", "ws://", 1)

	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, wsBase, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	feed := newWSFeed(conn)
	defer feed.stop()
	if _, err := feed.next(100 * time.Millisecond); err == nil {
		t.Fatal("nothing was sent yet, the first wait must time out")
	}
	close(release)
	m, err := feed.next(2 * time.Second)
	if err != nil || m["_type"] != "late" {
		t.Fatalf("the connection must survive a timed-out wait, got %v, %v", m, err)
	}
}
