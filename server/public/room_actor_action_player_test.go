package public

// The app-action event shape the realtime-frames contract publishes:
//
//	{type:'<ACTION>', ...payload, _by, player}
//
// Two invariants, both load-bearing and neither covered by the ctx/@lifecycle sets
// runtime_frames_drift_test.go pins:
//
//   - `player` is present and server-stamped. A reducer reads event.player.id in its
//     action cases exactly as it does in its lifecycle cases, so if the runtime omits
//     it there the first action throws, apply() drops the reduce, and the room stops
//     advancing state while every client keeps sending.
//   - the payload is spread at the TOP level. net.action(type, payload) sets
//     payload._action and sends payload itself, so a reducer reads event.x - never
//     event.data.x, which throws for the same silent result.
//
// Fixtures here build the frame the way ofnet.action() does. A fixture that wraps the
// payload in a key the client never sends would pass while every real build breaks.

import (
	"encoding/json"
	"testing"

	"openfabrica/db"
)

// idEchoReducer records the identity the runtime handed it for an app action. It reads
// event.player.id exactly as the contract prescribes - so it THROWS if player is absent,
// which is the defect under test.
const idEchoReducer = `
function reduce(state, event, ctx) {
  if (event.type === '@init') return { seen: '', uid: 0, forged: '', x: 0, wrapped: false };
  if (event.type === 'move') {
    return {
      seen: event.player.id,
      uid: event.player.uid || 0,
      forged: (event.player.meta || ''),
      x: event.x || 0,                                  // the payload spreads at the top level
      wrapped: Object.prototype.hasOwnProperty.call(event, 'data')
    };
  }
  return state;
}`

func newActionActor(t *testing.T) *roomActor {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	return newWritesActor(t, pdb, idEchoReducer)
}

func actionState(t *testing.T, a *roomActor) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.state, &m); err != nil {
		t.Fatalf("decode state %s: %v", a.state, err)
	}
	return m
}

func TestAppActionCarriesPlayerToReducer(t *testing.T) {
	a := newActionActor(t)
	a.apply(map[string]any{"type": "@init"}, "")
	a.uids["c1"] = 42

	// The frame ofnet.action('move', {x:1}) puts on the wire: payload fields at the
	// top level, plus the _action marker.
	a.handle(actorMsg{kind: amAction, client: "c1", frame: map[string]any{
		"_action": "move", "x": 1,
	}})

	st := actionState(t, a)
	if st["seen"] != "c1" {
		t.Fatalf("reducer saw event.player.id = %v, want \"c1\".\n"+
			"An app action reached the reducer without `player`, so `event.player.id` threw and the "+
			"reduce was dropped - the room stops applying input while every client keeps sending. "+
			"state=%s", st["seen"], a.state)
	}
	if uid, _ := st["uid"].(float64); uid != 42 {
		t.Errorf("event.player.uid = %v, want 42 - the action's player must be the same object "+
			"@join/@leave carry, or a reducer reads identity two different ways", st["uid"])
	}
}

// The payload reaches the reducer spread at the top level. Every doc that teaches an
// action's shape (the realtime-frames contract, manage_authoritative's set_reducer
// Example) must match this, because a reducer written against an event.data wrapper
// throws on its first action and the room silently stops advancing.
func TestAppActionPayloadIsSpreadAtTopLevel(t *testing.T) {
	a := newActionActor(t)
	a.apply(map[string]any{"type": "@init"}, "")

	a.handle(actorMsg{kind: amAction, client: "c1", frame: map[string]any{
		"_action": "move", "x": 7,
	}})

	st := actionState(t, a)
	if x, _ := st["x"].(float64); x != 7 {
		t.Errorf("reducer read event.x = %v, want 7 - net.action(type, payload) spreads the payload at the "+
			"top level of the event, so a reducer reads event.x", st["x"])
	}
	if wrapped, _ := st["wrapped"].(bool); wrapped {
		t.Error("the action event carries a `data` key - nothing in the client or the actor creates one, " +
			"so a doc teaching event.data.x would prescribe a throw. Keep the payload spread at the top level.")
	}
}

// The player object is server-stamped: a client that puts its own `player` in the frame
// must not be able to act as someone else.
func TestAppActionPlayerCannotBeForgedByTheClient(t *testing.T) {
	a := newActionActor(t)
	a.apply(map[string]any{"type": "@init"}, "")

	a.handle(actorMsg{kind: amAction, client: "c1", frame: map[string]any{
		"_action": "move",
		"player":  map[string]any{"id": "victim", "uid": 999, "meta": "spoofed"},
	}})

	st := actionState(t, a)
	if st["seen"] != "c1" {
		t.Errorf("reducer saw event.player.id = %v, want \"c1\" - a client-supplied `player` overrode "+
			"the server-stamped identity, letting any client act as any other", st["seen"])
	}
	if st["forged"] == "spoofed" {
		t.Error("a client-supplied player.meta survived into the reducer - the whole object must be " +
			"stamped from the connection")
	}
}

// A player's ?meta= profile reaches the reducer as an OBJECT on event.player.meta,
// not goja's byte-array wrapping of the raw stored JSON. The canonical set_reducer
// Example seats players with (p.meta && p.meta.name) on @join, so if meta arrives as
// a byte array every player silently reads back as the default name. ctx.players.meta
// already round-trips through JSON.parse; this pins that event.player.meta does too.
const metaEchoReducer = `
function reduce(state, event, ctx) {
  if (event.type === '@init') return { name: '', isArr: false };
  if (event.type === 'move') return {
    name: (event.player.meta && event.player.meta.name) || 'Guest',
    isArr: Array.isArray(event.player.meta)
  };
  return state;
}`

func TestAppActionCarriesParsedPlayerMeta(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	a := newWritesActor(t, pdb, metaEchoReducer)
	a.apply(map[string]any{"type": "@init"}, "")
	a.players["c1"] = json.RawMessage(`{"name":"Ada"}`)

	a.handle(actorMsg{kind: amAction, client: "c1", frame: map[string]any{"_action": "move"}})

	st := actionState(t, a)
	if isArr, _ := st["isArr"].(bool); isArr {
		t.Fatalf("event.player.meta reached the reducer as an array - the raw json.RawMessage was wrapped "+
			"as a byte array instead of parsed, so meta.<field> is always undefined. state=%s", a.state)
	}
	if st["name"] != "Ada" {
		t.Fatalf("event.player.meta.name = %v, want \"Ada\" - the ?meta= profile must reach the reducer as "+
			"an object, or every player reads back as the default. state=%s", st["name"], a.state)
	}
}

// @tick carries dt and no player, by contract: a sim ticks tens of times a second and
// there is no actor behind it. Pinned so the fix above cannot over-reach.
func TestTickCarriesNoPlayer(t *testing.T) {
	a := newActionActor(t)
	a.apply(map[string]any{"type": "@init"}, "")
	a.handle(actorMsg{kind: amTick, client: "", dt: 50})
	// A reducer that returns state unchanged on @tick leaves @init's zero value.
	if st := actionState(t, a); st["seen"] != "" {
		t.Errorf("@tick reached the reducer as something other than a player-less tick: %s", a.state)
	}
}
