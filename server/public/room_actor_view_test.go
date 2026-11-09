package public

import (
	"encoding/json"
	"testing"

	"openfabrica/db"
)

// viewActor wires a real hub with one joined client so sendViewTo actually
// delivers, and returns the actor plus a drain over that client's outbound frames.
func viewActor(t *testing.T, src string) (*roomActor, func() []map[string]any) {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	a := newWritesActor(t, pdb, src)
	hub := &wsHub{rooms: map[string]*wsRoom{}, actors: map[string]*roomActor{}}
	a.hub = hub
	client := hub.join(a.key, a.room, "p1", 0, nil, nil)
	// Seat the player directly: these tests drive apply() in-line, so there is no
	// actor goroutine to drain the mailbox a.join() would post to.
	a.players["p1"] = nil
	a.order = []string{"p1"}

	drain := func() []map[string]any {
		var out []map[string]any
		for {
			select {
			case msg := <-client.ch:
				var m map[string]any
				if err := json.Unmarshal(msg.data, &m); err == nil {
					out = append(out, m)
				}
			default:
				return out
			}
		}
	}
	return a, drain
}

func frameTypes(frames []map[string]any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		s, _ := f["_type"].(string)
		out = append(out, s)
	}
	return out
}

// view() is the ONLY thing between a player and everyone else's hidden state, so a
// projection that throws must send that player nothing at all. Falling back to the
// full state hands out every hand, secret role and unrevealed card, and does it
// silently: the room keeps running and nothing in the client looks wrong.
func TestViewFailureSendsNoStateFrame(t *testing.T) {
	a, drain := viewActor(t, `
	function reduce(state, event, ctx) {
	  if (event.type === '@init') return { secret: 'the hidden hand', public: 1 };
	  return state;
	}
	function view(state, playerId) {
	  throw new Error('projection bug');
	}`)

	a.apply(map[string]any{"type": "@init"}, "")
	frames := drain()

	for _, f := range frames {
		if f["_type"] == "state" {
			raw, _ := json.Marshal(f["state"])
			t.Fatalf("a throwing view() must not fall back to the full state, but a state frame was sent: %s", raw)
		}
	}
	sawFault := false
	for _, f := range frames {
		if f["_type"] == "error" && f["_fault"] == true {
			sawFault = true
		}
	}
	if !sawFault {
		t.Errorf("a held state frame must surface as a _fault error, got frames %v", frameTypes(frames))
	}
}

// The fallback is still right when the reducer defines NO view(): there is no
// hidden information to withhold, so the whole state IS every player's view.
func TestNoViewStillBroadcastsFullState(t *testing.T) {
	a, drain := viewActor(t, `
	function reduce(state, event, ctx) {
	  if (event.type === '@init') return { public: 1 };
	  return state;
	}`)

	a.apply(map[string]any{"type": "@init"}, "")
	frames := drain()

	for _, f := range frames {
		if f["_type"] == "state" {
			return
		}
	}
	t.Errorf("a reducer with no view() must still push state, got frames %v", frameTypes(frames))
}

// A working view() keeps projecting: the guard above must not cost hidden-info
// games their normal path.
func TestWorkingViewProjectsPerPlayer(t *testing.T) {
	a, drain := viewActor(t, `
	function reduce(state, event, ctx) {
	  if (event.type === '@init') return { secret: 'hidden', public: 7 };
	  return state;
	}
	function view(state, playerId) {
	  return { public: state.public };
	}`)

	a.apply(map[string]any{"type": "@init"}, "")
	for _, f := range drain() {
		if f["_type"] != "state" {
			continue
		}
		raw, _ := json.Marshal(f["state"])
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("state frame: %v", err)
		}
		if _, leaked := got["secret"]; leaked {
			t.Errorf("view() output leaked the hidden field: %s", raw)
		}
		if got["public"] == nil {
			t.Errorf("view() output lost the public field: %s", raw)
		}
		return
	}
	t.Error("a working view() must still push a state frame")
}
