package public

import (
	"testing"
)

// A reload gives every live room of the endpoint its own compiled reducer: a
// reducer owns a JS runtime that only one actor goroutine may use.
func TestReloadEndpointReducerCompilesPerRoom(t *testing.T) {
	_, pdb := spineDispatchDB(t)
	mustExec(t, pdb.Writer(), `INSERT INTO of_ws_endpoints (path, model, authoritative, reducer) VALUES ('game', 'authoritative', 1, 'function reduce(s, e, ctx) { return s || {} }')`)

	hub := NewWSHub()
	for _, key := range []string{"1:0:game:a", "1:0:game:b"} {
		hub.actors[key] = &roomActor{key: key, endpoint: "game", reducerSrc: "old", pdb: pdb, mailbox: make(chan actorMsg, 4), stopCh: make(chan struct{})}
	}
	hub.ReloadEndpointReducer(1, "game")

	var got []actorMsg
	for _, a := range hub.actors {
		select {
		case m := <-a.mailbox:
			got = append(got, m)
		default:
			t.Fatalf("room %s received no reload", a.key)
		}
	}
	if got[0].kind != amReload || got[1].kind != amReload {
		t.Fatalf("kinds = %v, %v; want two reloads", got[0].kind, got[1].kind)
	}
	if got[0].reducer == nil || got[0].reducer == got[1].reducer {
		t.Fatal("two rooms were handed the same reducer runtime")
	}
}
