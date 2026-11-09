package roomactor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustCompile(t *testing.T, code string) *Reducer {
	t.Helper()
	r, err := Compile(code)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return r
}

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return m
}

const counterReducer = `
function reduce(state, event, ctx) {
  if (event.type === '@init') return { count: 0 };
  if (event.type === 'inc')   return { count: state.count + (event.by || 1) };
  if (event.type === 'bad')   { ctx.reject('not allowed'); }
  return state;
}`

func TestCompileRequiresReduce(t *testing.T) {
	if _, err := Compile(`function notReduce(){}`); err == nil {
		t.Fatal("expected error when reduce() is missing")
	}
	if _, err := Compile(`function reduce(s,e,c){ this is not js `); err == nil {
		t.Fatal("expected parse error for invalid JS")
	}
}

func TestReduceInitAndAction(t *testing.T) {
	r := mustCompile(t, counterReducer)

	res, err := r.Reduce(nil, map[string]any{"type": "@init"}, nil, 1, 0)
	if err != nil {
		t.Fatalf("@init: %v", err)
	}
	if decode(t, res.State)["count"].(float64) != 0 {
		t.Fatalf("@init state = %s, want count 0", res.State)
	}

	res2, err := r.Reduce(res.State, map[string]any{"type": "inc", "by": 2}, nil, 1, 0)
	if err != nil {
		t.Fatalf("inc: %v", err)
	}
	if decode(t, res2.State)["count"].(float64) != 2 {
		t.Fatalf("inc state = %s, want count 2", res2.State)
	}
}

// A reducer that puts a non-finite number (NaN or Infinity) in state cannot
// serialize. The failure must name it so the fault is diagnosable rather than a raw
// encoder error, and it must never commit unserializable state.
func TestReduceNonFiniteStateFailsClearly(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ return { x: 0/0 }; }`)
	if _, err := r.Reduce(nil, map[string]any{"type": "@init"}, nil, 1, 0); err == nil {
		t.Fatal("a reducer producing NaN must fail, not commit unserializable state")
	} else if !strings.Contains(err.Error(), "non-finite") {
		t.Errorf("error must name the non-finite value so it is diagnosable, got: %v", err)
	}
}

// ctx.after(name, seconds) from model code must be clamped: a NaN or negative delay
// would fire immediately and an absurd one overflows the millisecond conversion, so
// the stored delay stays within [0, one day].
func TestReduceAfterClampsDelay(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){
	  if (e.type === 'neg')  c.after('t', -5);
	  if (e.type === 'huge') c.after('t', 1e18);
	  if (e.type === 'ok')   c.after('t', 30);
	  return s || {};
	}`)
	cases := []struct {
		ev     string
		wantMS int
	}{
		{"neg", 0},
		{"huge", 86400 * 1000},
		{"ok", 30 * 1000},
	}
	for _, tc := range cases {
		res, err := r.Reduce(json.RawMessage("{}"), map[string]any{"type": tc.ev}, nil, 1, 0)
		if err != nil {
			t.Fatalf("%s: reduce: %v", tc.ev, err)
		}
		if len(res.Timers) != 1 {
			t.Fatalf("%s: want 1 timer, got %d", tc.ev, len(res.Timers))
		}
		if res.Timers[0].MS != tc.wantMS {
			t.Errorf("%s: after MS = %d, want %d", tc.ev, res.Timers[0].MS, tc.wantMS)
		}
	}
}

// TestReduceArrayMutationRoundTrips guards state deserialization: in-place
// mutations (array push, added object key) must survive Export and re-enter the
// next reduce; a lost push shows up as an empty array on the following event.
func TestReduceArrayMutationRoundTrips(t *testing.T) {
	r := mustCompile(t, `
	function reduce(state, event, ctx) {
	  if (event.type === '@init' || !state) return { items: [], byId: {} };
	  if (event.type === 'add') {
	    state.items.push(event.id);
	    state.byId[event.id] = { seen: true };
	  }
	  return state;
	}`)

	res, err := r.Reduce(nil, map[string]any{"type": "@init"}, nil, 1, 0)
	if err != nil {
		t.Fatalf("@init: %v", err)
	}
	for i, id := range []string{"a", "b"} {
		res, err = r.Reduce(res.State, map[string]any{"type": "add", "id": id}, nil, uint64(i+2), 0)
		if err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}

	var st struct {
		Items []string       `json:"items"`
		ByID  map[string]any `json:"byId"`
	}
	if err := json.Unmarshal(res.State, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(st.Items) != 2 || st.Items[0] != "a" || st.Items[1] != "b" {
		t.Fatalf("array push did not survive Export: items = %v, want [a b]", st.Items)
	}
	if len(st.ByID) != 2 {
		t.Fatalf("object keys = %d, want 2", len(st.ByID))
	}
}

// The same round trip on the OTHER side of the call, pinned because it holds by
// virtue of how goja wraps Go values rather than by anything this package does.
// If a wrapper ever stops growing on push, the append vanishes silently - no
// gate fires - so this asserts the property directly instead of trusting it.
func TestReduceEventValuesRoundTripMutations(t *testing.T) {
	r := mustCompile(t, `
	function reduce(state, event, ctx) {
	  if (event.type === '@init' || !state) return {};
	  if (event.type === 'deal') {
	    state.board = event.shared;            // lifted straight out of the event
	    state.board.push({ id: 'local' });     // then appended to in place
	    state.hand = event.cards;
	    state.hand.push('drawn');
	    state.seats = ctx.players.map(function (p) { return p.id; });
	  }
	  return state;
	}`)

	res, err := r.Reduce(nil, map[string]any{"type": "@init"}, nil, 1, 0)
	if err != nil {
		t.Fatalf("@init: %v", err)
	}
	players := []Player{{ID: "p1"}, {ID: "p2"}}
	event := map[string]any{
		"type":   "deal",
		"cards":  []any{"AS", "KD"},
		"shared": []map[string]any{{"id": "row1"}},
	}
	res, err = r.Reduce(res.State, event, players, 2, 0)
	if err != nil {
		t.Fatalf("deal: %v", err)
	}

	var st struct {
		Board []struct {
			ID string `json:"id"`
		} `json:"board"`
		Hand  []string `json:"hand"`
		Seats []string `json:"seats"`
	}
	if err := json.Unmarshal(res.State, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(st.Board) != 2 || st.Board[1].ID != "local" {
		t.Fatalf("push onto an event.shared-derived array did not survive Export: board = %+v", st.Board)
	}
	if len(st.Hand) != 3 || st.Hand[2] != "drawn" {
		t.Fatalf("push onto an event-derived array did not survive Export: hand = %v, want [AS KD drawn]", st.Hand)
	}
	if len(st.Seats) != 2 || st.Seats[0] != "p1" {
		t.Fatalf("ctx.players.map = %v, want [p1 p2]", st.Seats)
	}
}

func TestReduceUpsertWrite(t *testing.T) {
	r := mustCompile(t, `
	function reduce(state, event, ctx) {
	  if (event.type === 'place') {
	    ctx.upsert('world_edits', { pos: event.pos, block: event.block }, ['pos']);
	  }
	  return state;
	}`)
	res, err := r.Reduce(json.RawMessage(`{}`),
		map[string]any{"type": "place", "pos": "5,10,5", "block": "stone"}, nil, 1, 0)
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if len(res.Writes) != 1 {
		t.Fatalf("got %d writes, want 1", len(res.Writes))
	}
	w := res.Writes[0]
	if w.Op != "upsert" || w.Table != "world_edits" {
		t.Fatalf("write = %+v, want op=upsert table=world_edits", w)
	}
	if len(w.Conflict) != 1 || w.Conflict[0] != "pos" {
		t.Fatalf("conflict = %v, want [pos] (goja must marshal the JS array into []string)", w.Conflict)
	}
	if w.Row["pos"] != "5,10,5" || w.Row["block"] != "stone" {
		t.Fatalf("row = %v, want pos=5,10,5 block=stone", w.Row)
	}
}

func TestReduceRejectIsClean(t *testing.T) {
	r := mustCompile(t, counterReducer)
	_, err := r.Reduce(json.RawMessage(`{"count":5}`), map[string]any{"type": "bad"}, nil, 1, 0)
	var rej *Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("expected *Rejection, got %v", err)
	}
	if rej.Msg != "not allowed" {
		t.Fatalf("reject msg = %q, want 'not allowed'", rej.Msg)
	}
}

// A runaway loop in top-level code must fail Compile within CompileBudget instead
// of pinning the goroutine that joins the room.
func TestCompileTopLevelLoopTimesOut(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := Compile(`while(true){} function reduce(s,e,c){ return s; }`)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "top-level code exceeded") {
			t.Fatalf("expected a top-level budget error, got %v", err)
		}
	case <-time.After(5 * CompileBudget):
		t.Fatal("Compile did not return: top-level code ran without a time budget")
	}
}

func TestReduceTimeoutInterrupts(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ while(true){} }`)
	_, err := r.Reduce(nil, map[string]any{"type": "@init"}, nil, 1, 0)
	var to *Timeout
	if !errors.As(err, &to) {
		t.Fatalf("expected *Timeout for an infinite loop, got %v", err)
	}
}

func TestCtxRandomDeterministic(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ return { r: c.random() }; }`)
	a, _ := r.Reduce(nil, map[string]any{"type": "x"}, nil, 42, 0)
	b, _ := r.Reduce(nil, map[string]any{"type": "x"}, nil, 42, 0)
	c, _ := r.Reduce(nil, map[string]any{"type": "x"}, nil, 43, 0)
	ra := decode(t, a.State)["r"].(float64)
	rb := decode(t, b.State)["r"].(float64)
	rc := decode(t, c.State)["r"].(float64)
	if ra != rb {
		t.Fatalf("same seed gave different randoms: %v vs %v", ra, rb)
	}
	if ra < 0 || ra >= 1 {
		t.Fatalf("random out of [0,1): %v", ra)
	}
	if ra == rc {
		t.Fatalf("different seeds gave identical random %v (suspicious)", ra)
	}
}

func TestCtxNowInjected(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ return { t: c.now() }; }`)
	res, err := r.Reduce(nil, map[string]any{"type": "x"}, nil, 1, 12345)
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, res.State)["t"].(float64) != 12345 {
		t.Fatalf("ctx.now() = %s, want 12345", res.State)
	}
}

func TestCtxPlayers(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ return { n: c.players.length, first: c.players[0].meta.name }; }`)
	players := []Player{{ID: "p1", Meta: json.RawMessage(`{"name":"Ada"}`)}}
	res, err := r.Reduce(nil, map[string]any{"type": "x"}, players, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, res.State)
	if m["n"].(float64) != 1 || m["first"].(string) != "Ada" {
		t.Fatalf("ctx.players wrong: %s", res.State)
	}
}

func TestSetTimerRecorded(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ c.after('turn', 5); return {}; }`)
	res, err := r.Reduce(nil, map[string]any{"type": "x"}, nil, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Timers) != 1 || res.Timers[0].Name != "turn" || !res.Timers[0].Set || res.Timers[0].MS != 5000 {
		t.Fatalf("timer op not recorded: %+v", res.Timers)
	}
}

func TestCtxPlayerUID(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ return { uid: c.players[0].uid }; }`)
	players := []Player{{ID: "u7", UID: 7, Meta: json.RawMessage(`{"name":"Ada"}`)}}
	res, err := r.Reduce(nil, map[string]any{"type": "x"}, players, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decode(t, res.State)["uid"].(float64) != 7 {
		t.Fatalf("ctx.players[0].uid wrong: %s", res.State)
	}
}

func TestCtxInsertRecorded(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ c.insert('hand_history', { player_id: 7, net: 150 }); return {}; }`)
	res, err := r.Reduce(nil, map[string]any{"type": "settle"}, nil, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Writes) != 1 {
		t.Fatalf("want 1 write, got %d", len(res.Writes))
	}
	w := res.Writes[0]
	if w.Op != "insert" || w.Table != "hand_history" || w.Where != nil {
		t.Fatalf("insert write malformed: %+v", w)
	}
	if w.Row["player_id"].(int64) != 7 || w.Row["net"].(int64) != 150 {
		t.Fatalf("insert row wrong: %+v", w.Row)
	}
}

func TestCtxUpdateRecorded(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ c.update('players', { bankroll: { '$increment': 150 } }, { id: 7 }); return {}; }`)
	res, err := r.Reduce(nil, map[string]any{"type": "settle"}, nil, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Writes) != 1 {
		t.Fatalf("want 1 write, got %d", len(res.Writes))
	}
	w := res.Writes[0]
	if w.Op != "update" || w.Table != "players" {
		t.Fatalf("update write malformed: %+v", w)
	}
	patch, ok := w.Row["bankroll"].(map[string]any)
	if !ok || patch["$increment"].(int64) != 150 {
		t.Fatalf("update patch wrong: %+v", w.Row)
	}
	if w.Where["id"].(int64) != 7 {
		t.Fatalf("update where wrong: %+v", w.Where)
	}
}

func TestWritesCapEnforced(t *testing.T) {
	src := "function reduce(s,e,c){ for (var i=0;i<" + itoa(MaxWrites+1) + ";i++) c.insert('t', {i:i}); return {}; }"
	r := mustCompile(t, src)
	if _, err := r.Reduce(nil, map[string]any{"type": "x"}, nil, 1, 0); err == nil {
		t.Fatalf("expected error when a reduce declares more than %d writes", MaxWrites)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestAmbientNondeterminismBlocked(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,c){ return { x: Math.random() }; }`)
	_, err := r.Reduce(nil, map[string]any{"type": "x"}, nil, 1, 0)
	if err == nil {
		t.Fatal("expected Math.random() to be blocked")
	}
	var rej *Rejection
	if errors.As(err, &rej) {
		t.Fatalf("blocked global should be a thrown error, not a rejection: %v", err)
	}
}

func TestViewHidesOtherPlayersSecrets(t *testing.T) {
	r := mustCompile(t, `
function reduce(state, event, ctx) {
  if (event.type === '@init') return { hands: { a: 'AA', b: 'KK' } };
  return state;
}
function view(state, pid) {
  var out = { hands: {} };
  for (var k in state.hands) { out.hands[k] = (k === pid) ? state.hands[k] : 'hidden'; }
  return out;
}`)
	if !r.HasView() {
		t.Fatal("HasView() should be true when view() is defined")
	}
	res, err := r.Reduce(nil, map[string]any{"type": "@init"}, nil, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	va, err := r.View(res.State, "a")
	if err != nil {
		t.Fatal(err)
	}
	hands := decode(t, va)["hands"].(map[string]any)
	if hands["a"].(string) != "AA" {
		t.Fatalf("player a should see own hand AA, got %v", hands["a"])
	}
	if hands["b"].(string) != "hidden" {
		t.Fatalf("player a must NOT see b's hand, got %v (hidden-info leak)", hands["b"])
	}
}

func TestNoViewReturnsFullState(t *testing.T) {
	r := mustCompile(t, counterReducer)
	if r.HasView() {
		t.Fatal("HasView() should be false")
	}
	full := json.RawMessage(`{"count":7}`)
	out, err := r.View(full, "anyone")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(full) {
		t.Fatalf("no-view projection = %s, want full state %s", out, full)
	}
}
