package public

// The authoritative model exists to keep authority off the client. It must close two
// holes in the wire:
//
//   - `_action` must not be copied straight into event.type, or a client could send
//     {_action:"@timer", name:"settle"} and fire the reducer's settlement branch, a
//     forged @tick with any dt, or @init to reset the room.
//   - event.shared is server-injected context, so apply() must always overwrite it, not
//     only when a shared read returns rows, or a client-supplied `shared` reaches the reducer.
//
// And one roster invariant: a reconnect (same client id) re-posts amJoin while the old
// slot is still held, which must not append the id to the roster a second time.

import (
	"encoding/json"
	"testing"

	"openfabrica/db"
)

const forgeReducer = `
function reduce(state, event, ctx) {
  if (event.type === '@init') return { settled: false, ticks: 0, sharedSeen: false, acts: 0, joins: 0, roster: 0 };
  if (event.type === '@timer' && event.name === 'settle') { state.settled = true; return state; }
  if (event.type === '@tick') { state.ticks++; return state; }
  if (event.type === '@join') { state.joins++; state.roster = ctx.players.length; return state; }
  if (event.type === 'poke') { state.acts++; if (event.shared !== undefined) state.sharedSeen = true; state.roster = ctx.players.length; return state; }
  return state;
}`

func newForgeActor(t *testing.T) *roomActor {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	a := newWritesActor(t, pdb, forgeReducer)
	a.hub = NewWSHub() // sendErr/sendViewTo route through the hub; an empty room is a no-op
	a.apply(map[string]any{"type": "@init"}, "")
	return a
}

func forgeState(t *testing.T, a *roomActor) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.state, &m); err != nil {
		t.Fatalf("decode state %s: %v", a.state, err)
	}
	return m
}

func TestClientCannotForgeLifecycleEvents(t *testing.T) {
	for _, forged := range []map[string]any{
		{"_action": "@timer", "name": "settle"},
		{"_action": "@tick", "dt": 999},
		{"_action": "@init"},
		{"_action": "@join"},
		{"_action": ""},
		{"_action": 7},
	} {
		a := newForgeActor(t)
		a.handle(actorMsg{kind: amAction, client: "c1", frame: forged})
		st := forgeState(t, a)
		if st["settled"] == true || st["ticks"] != float64(0) || st["joins"] != float64(0) {
			t.Errorf("frame %v reached the reducer as a lifecycle event (state %s) - '@' events are server-only", forged, a.state)
		}
	}
}

func TestAppActionStillApplies(t *testing.T) {
	a := newForgeActor(t)
	a.handle(actorMsg{kind: amAction, client: "c1", frame: map[string]any{"_action": "poke"}})
	if st := forgeState(t, a); st["acts"] != float64(1) {
		t.Fatalf("a legitimate app action was not applied: %s", a.state)
	}
}

func TestClientCannotForgeSharedContext(t *testing.T) {
	a := newForgeActor(t) // no shared_table configured, so readShared returns nil
	a.handle(actorMsg{kind: amAction, client: "c1", frame: map[string]any{
		"_action": "poke", "shared": []any{map[string]any{"pool": 1e9}},
	}})
	if st := forgeState(t, a); st["sharedSeen"] == true {
		t.Fatal("a client-supplied `shared` reached the reducer - event.shared must be server-injected only")
	}
}

func TestReconnectKeepsOneRosterEntry(t *testing.T) {
	a := newForgeActor(t)
	a.handle(actorMsg{kind: amJoin, client: "c1"})
	a.handle(actorMsg{kind: amJoin, client: "c2"})
	a.handle(actorMsg{kind: amJoin, client: "c1"}) // same stable id reconnecting

	if len(a.order) != 2 {
		t.Fatalf("roster after reconnect = %v, want [c1 c2] - a reconnect must not add a second entry", a.order)
	}
	a.handle(actorMsg{kind: amAction, client: "c2", frame: map[string]any{"_action": "poke"}})
	if st := forgeState(t, a); st["roster"] != float64(2) {
		t.Errorf("ctx.players.length = %v, want 2", st["roster"])
	}

	a.handle(actorMsg{kind: amLeave, client: "c1"})
	for _, id := range a.order {
		if id == "c1" {
			t.Fatalf("c1 still in the roster after leaving: %v (ghost player)", a.order)
		}
	}
}
