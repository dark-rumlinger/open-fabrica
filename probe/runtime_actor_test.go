package probe

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func actorSmokeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_ws_endpoints (path TEXT, reducer TEXT, authoritative INTEGER DEFAULT 0, tick_hz INTEGER DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func addEndpoint(t *testing.T, db *sql.DB, path, reducer string, authoritative int) {
	t.Helper()
	addTickEndpoint(t, db, path, reducer, authoritative, 0)
}

func addTickEndpoint(t *testing.T, db *sql.DB, path, reducer string, authoritative, tickHz int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_ws_endpoints (path, reducer, authoritative, tick_hz) VALUES (?, ?, ?, ?)`, path, reducer, authoritative, tickHz); err != nil {
		t.Fatal(err)
	}
}

// TestAuthoritativeReducerSmoke pins the build-end probe: a healthy reducer is
// clean, while a reducer that throws on @init, loops forever, has a throwing
// view(), or is missing entirely each becomes one deterministic issue - and a
// non-authoritative endpoint with junk in the column is never probed.
func TestAuthoritativeReducerSmoke(t *testing.T) {
	db := actorSmokeDB(t)

	addEndpoint(t, db, "ok", `function reduce(s,e,c){ if(e.type==='@init') return {n:0}; return s; }`, 1)
	addEndpoint(t, db, "throws", `function reduce(s,e,c){ if(e.type==='@init'){ throw new Error('boom'); } return s; }`, 1)
	addEndpoint(t, db, "loops", `function reduce(s,e,c){ while(true){} }`, 1)
	addEndpoint(t, db, "noreducer", ``, 1)
	addEndpoint(t, db, "badview", `function reduce(s,e,c){ return {x:1}; } function view(s,p){ throw new Error('leak'); }`, 1)
	addEndpoint(t, db, "relay", `this is not js`, 0) // non-authoritative: must be ignored

	issues := RunAuthoritativeReducerSmoke(context.Background(), db)
	byPage := map[string]ValidationIssue{}
	for _, i := range issues {
		byPage[i.Page] = i
	}
	if len(issues) != 4 {
		t.Fatalf("got %d issues, want 4: %+v", len(issues), issues)
	}
	if _, flagged := byPage["/api/ok/ws"]; flagged {
		t.Error("a healthy reducer was flagged")
	}
	if _, flagged := byPage["/api/relay/ws"]; flagged {
		t.Error("a non-authoritative endpoint was probed")
	}
	if m := byPage["/api/throws/ws"].Message; !strings.Contains(m, "@init") {
		t.Errorf("throws issue should name @init: %s", m)
	}
	if m := byPage["/api/loops/ws"].Message; !strings.Contains(m, "timed out") {
		t.Errorf("loops issue should report a timeout: %s", m)
	}
	if m := byPage["/api/noreducer/ws"].Message; !strings.Contains(m, "no reducer") {
		t.Errorf("missing-reducer issue should say so: %s", m)
	}
	if m := byPage["/api/badview/ws"].Message; !strings.Contains(m, "view()") {
		t.Errorf("bad-view issue should name view(): %s", m)
	}
}

// reducerOverApplied reproduces a shape that leaves an authoritative game dead:
// a 1-parameter helper called with two arguments, so the argument it actually
// wanted lands in the second slot and the first parameter binds to a number. JS
// drops the extra silently, the reducer parses, and set_reducer saves it green.
const reducerOverApplied = `function randDir(r) { return r.random() < 0.5 ? -1 : 1; }
function reduce(state, event, ctx) {
  if (event.type === '@init') { return { dir: randDir(1, ctx) }; }
  return state;
}`

// TestReducerArityIsFlagged pins the static arity pass over reducer source. The
// reducer is a DB column, so no other gate in the platform ever reads it - this is
// the only thing standing between an over-applied call and a shipped game.
func TestReducerArityIsFlagged(t *testing.T) {
	db := actorSmokeDB(t)
	addEndpoint(t, db, "pong", reducerOverApplied, 1)

	var arity *ValidationIssue
	for _, is := range RunAuthoritativeReducerSmoke(context.Background(), db) {
		if is.Category == "arity_mismatch" {
			c := is
			arity = &c
		}
	}
	if arity == nil {
		t.Fatal("an over-applied call in a reducer must be flagged")
	}
	if arity.Severity != "warning" || !arity.Deterministic {
		t.Errorf("a reducer arity mismatch is a deterministic warning (usually an ignored extra argument): %+v", arity)
	}
	if !strings.Contains(arity.Message, "randDir") {
		t.Errorf("message should name the over-applied function, got: %s", arity.Message)
	}
	// The steer is what makes the finding actionable: the model cannot open a
	// reducer as a file, so a finding that does not name the tool is a dead end.
	if !strings.Contains(arity.Message, "action=get") {
		t.Errorf("message should tell the model how to read the reducer, got: %s", arity.Message)
	}
}

// TestReducerArityCleanIsSilent proves the pass does not fire once the call passes
// its argument in the slot the definition declares. Correct reducers must measure
// clean, so a false positive here would block good games.
func TestReducerArityCleanIsSilent(t *testing.T) {
	db := actorSmokeDB(t)
	addEndpoint(t, db, "ok", `function randDir(r) { return r.random() < 0.5 ? -1 : 1; }
function reduce(state, event, ctx) {
  if (event.type === '@init') { return { dir: randDir(ctx) }; }
  return state;
}`, 1)

	for _, is := range RunAuthoritativeReducerSmoke(context.Background(), db) {
		t.Errorf("a correct reducer was flagged: %+v", is)
	}
}

// TestReducerArityAndSmokeAreDistinctFindings pins that the two passes over ONE
// endpoint stay separately fixable. The over-applied call is the CAUSE of the @init
// throw, so both fire together on exactly the reducer this reproduces; if they
// shared a Fingerprint() the loop would present one and silently demote the other,
// and the one it dropped names the actual line.
func TestReducerArityAndSmokeAreDistinctFindings(t *testing.T) {
	db := actorSmokeDB(t)
	addEndpoint(t, db, "pong", reducerOverApplied, 1)

	issues := RunAuthoritativeReducerSmoke(context.Background(), db)
	if len(issues) != 2 {
		t.Fatalf("want both an arity finding and a lifecycle finding, got %d: %+v", len(issues), issues)
	}
	if issues[0].Fingerprint() == issues[1].Fingerprint() {
		t.Fatal("the arity and lifecycle findings on one endpoint must not share a fingerprint")
	}
	if issues[0].Category != "arity_mismatch" {
		t.Errorf("the arity finding should lead: it names the line, the throw names only the event. got %q", issues[0].Category)
	}
}

// reducerActionOnlyUnbound reproduces a helper that
// calls ctx.random() without taking ctx as a parameter, reached ONLY from a player
// action (`serve`). ctx is the third parameter of reduce(), not a global, so the
// call throws `ctx is not defined` the instant a serve runs - but the lifecycle run
// never synthesizes player actions, so without the static pass this ships green and
// freezes the room on the first serve.
const reducerActionOnlyUnbound = `function resetBall(s, dir) { s.ball = { vx: ctx.random(), dir: dir }; return s; }
function reduce(state, event, ctx) {
  if (event.type === '@init') { return { phase: 'lobby', ball: null }; }
  if (event.type === 'serve') { return resetBall(state, event.dir); }
  return state;
}`

// TestReducerUnboundCtxIsFlagged pins the static unbound-reference pass on exactly
// the class the run cannot see: a helper reachable only from an app action. The run
// alone returns clean here, so this static finding is the only thing standing
// between the break and a shipped, freeze-on-first-serve game.
func TestReducerUnboundCtxIsFlagged(t *testing.T) {
	db := actorSmokeDB(t)
	addEndpoint(t, db, "pong", reducerActionOnlyUnbound, 1)

	issues := RunAuthoritativeReducerSmoke(context.Background(), db)
	// The run never drives `serve`, so the ONLY finding is the static unbound-ref one.
	if len(issues) != 1 {
		t.Fatalf("want exactly the unbound-ref finding (the run alone is blind to it), got %d: %+v", len(issues), issues)
	}
	iss := issues[0]
	if iss.Category != "undefined_reference" {
		t.Errorf("category should be undefined_reference, got %q", iss.Category)
	}
	if iss.Severity != "error" || !iss.Deterministic {
		t.Errorf("an unbound reference in a reducer must be a deterministic error so it drives a fix-pass: %+v", iss)
	}
	if !strings.Contains(iss.Message, "ctx") {
		t.Errorf("message should name the unbound identifier, got: %s", iss.Message)
	}
	// The steer must name the tool: the model cannot open a reducer as a file, so a
	// finding that does not say how to read it is a dead end (as with reducerArity).
	if !strings.Contains(iss.Message, "action=get") {
		t.Errorf("message should tell the model how to read the reducer, got: %s", iss.Message)
	}
}

// TestReducerUnboundCleanIsSilent proves the pass stays quiet once the helper takes
// ctx as a parameter and every caller threads it. A false positive here would block
// a correct game, which is worse than missing a broken one - the same bias the
// undefined-ref analyzer carries everywhere else.
func TestReducerUnboundCleanIsSilent(t *testing.T) {
	db := actorSmokeDB(t)
	addEndpoint(t, db, "ok", `function resetBall(s, ctx, dir) { s.ball = { vx: ctx.random(), dir: dir }; return s; }
function reduce(state, event, ctx) {
  if (event.type === '@init') { return { phase: 'lobby', ball: null }; }
  if (event.type === 'serve') { return resetBall(state, ctx, event.dir); }
  return state;
}`, 1)

	for _, is := range RunAuthoritativeReducerSmoke(context.Background(), db) {
		t.Errorf("a correct reducer was flagged: %+v", is)
	}
}

// TestReducerUnboundAndSmokeAreDistinctFindings pins that when the unbound helper IS
// reachable from a platform event (@join here), the static pass and the lifecycle
// run both fault on it and stay separately fixable - a shared Fingerprint() would let
// one demote the other in the fix loop, and each names a different half of the fix
// (the static one the line, the run the event).
func TestReducerUnboundAndSmokeAreDistinctFindings(t *testing.T) {
	db := actorSmokeDB(t)
	addEndpoint(t, db, "pong", `function startRound(s) { s.seed = ctx.random(); return s; }
function reduce(state, event, ctx) {
  if (event.type === '@init') { return { phase: 'lobby' }; }
  if (event.type === '@join') { if (ctx.players.length === 2) { return startRound(state); } return state; }
  return state;
}`, 1)

	issues := RunAuthoritativeReducerSmoke(context.Background(), db)
	if len(issues) != 2 {
		t.Fatalf("want both a static unbound finding and a lifecycle finding, got %d: %+v", len(issues), issues)
	}
	if issues[0].Fingerprint() == issues[1].Fingerprint() {
		t.Fatal("the unbound and lifecycle findings on one endpoint must not share a fingerprint")
	}
	if issues[0].Category != "undefined_reference" {
		t.Errorf("the static unbound finding should lead: it names the line, the throw names only the event. got %q", issues[0].Category)
	}
}

// stallReducer reproduces a stall that leaves a game dead: a countdown driven by
// a one-shot ctx.after whose @timer branch decrements but never re-arms. It runs
// clean forever - every event returns valid state - and the room never leaves the
// countdown.
const stallReducer = `function reduce(s, e, c) {
  if (e.type === '@init')  { return { phase: 'lobby', left: 3 }; }
  if (e.type === '@join')  { if (c.players.length === 2) { s.phase = 'countdown'; c.after('tick1', 1); } return s; }
  if (e.type === '@timer') { s.left -= 1; return s; }
  return s;
}`

// TestReducerStallIsFlagged pins the stall pass. Nothing faults here: this reducer
// satisfies every other check the platform has, which is why only the stall pass
// can catch it.
func TestReducerStallIsFlagged(t *testing.T) {
	iss := smokeReducer(authEndpoint{path: "arena", reducer: stallReducer, tickHz: 30})
	if iss == nil {
		t.Fatal("a ticking room that advances and then cannot resume must be flagged")
	}
	if !strings.Contains(iss.Signature, "inert") {
		t.Errorf("a stall should carry its own phase Signature, got %q", iss.Signature)
	}
	// The steer has to name the fix, because the symptom (a stuck countdown) does
	// not point at the cause (a timer that fires once).
	if !strings.Contains(iss.Message, "re-arm") && !strings.Contains(iss.Message, "Re-arm") {
		t.Errorf("the stall message should say how to fix it, got: %s", iss.Message)
	}
}

// TestReducerStallFalsePositives is the important half. Each of these rooms also
// sits still, and each is CORRECT; a probe that flagged any of them would block a
// good build, which is worse than missing a bad one.
func TestReducerStallFalsePositives(t *testing.T) {
	cases := []struct {
		name    string
		reducer string
		tickHz  int
	}{
		{
			// A lobby waiting for more players than the probe seats. It never moves
			// under tick at all, so it never "started" and is not judged.
			name:   "lobby waiting for a 4th player",
			tickHz: 30,
			reducer: `function reduce(s, e, c) {
  if (e.type === '@init') { return { phase: 'waiting' }; }
  if (e.type === '@join' && c.players.length === 4) { s.phase = 'play'; }
  return s;
}`,
		},
		{
			// Turn-based: tick_hz = 0, so it has no sim to advance and sitting still
			// between moves is the whole design.
			name:   "turn-based room between moves",
			tickHz: 0,
			reducer: `function reduce(s, e, c) {
  if (e.type === '@init') { return { turn: 0 }; }
  return s;
}`,
		},
		{
			// Still counting down, with the timer re-armed. It is mid-transition when
			// the run ends, and a pending timer means it can still resume.
			name:   "countdown that re-arms its timer",
			tickHz: 30,
			reducer: `function reduce(s, e, c) {
  if (e.type === '@init')  { return { phase: 'lobby', left: 999 }; }
  if (e.type === '@join')  { c.after('t', 1); return s; }
  if (e.type === '@timer') { s.left -= 1; c.after('t', 1); return s; }
  return s;
}`,
		},
		{
			// A healthy sim: @tick moves the state every frame, forever.
			name:   "sim advancing every tick",
			tickHz: 30,
			reducer: `function reduce(s, e, c) {
  if (e.type === '@init') { return { x: 0 }; }
  if (e.type === '@tick') { s.x += e.dt; return s; }
  return s;
}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if iss := smokeReducer(authEndpoint{path: "g", reducer: c.reducer, tickHz: c.tickHz}); iss != nil {
				t.Errorf("a correct reducer was flagged: %s", iss.Message)
			}
		})
	}
}

// TestLifecycleTimersAreDelivered pins that a timer armed during @init reaches the
// reducer. The actor applies timers after every successful reduce, so a probe that
// carried only the last @join's would silently never fire an @init turn clock - and
// the @timer branch it guards is precisely what this pass exists to exercise.
func TestLifecycleTimersAreDelivered(t *testing.T) {
	// Throws only if the @init-armed timer is actually delivered. If the probe drops
	// it, this reducer passes and the test fails.
	iss := smokeReducer(authEndpoint{path: "g", tickHz: 0, reducer: `function reduce(s, e, c) {
  if (e.type === '@init') { c.after('boot', 1); return {}; }
  if (e.type === '@timer' && e.name === 'boot') { throw new Error('boot timer fired'); }
  return s;
}`})
	if iss == nil {
		t.Fatal("a timer armed in @init must be delivered during the run")
	}
	if !strings.Contains(iss.Message, "@timer") {
		t.Errorf("the fault should be attributed to the @timer event, got: %s", iss.Message)
	}
}

// TestReducerSmokeSignatureNamesThePhase pins the phase-specific Signature:
// a reducer-smoke issue carries "reducer:<phase>" so (a) it never shares a
// Fingerprint() with the runtime_ws welcome smoke on the same endpoint, and (b)
// a fix that clears one lifecycle phase and reveals the next earns its own
// fix-pass instead of being demoted as a survivor of the first.
func TestReducerSmokeSignatureNamesThePhase(t *testing.T) {
	initThrow := smokeReducer(authEndpoint{path: "g",
		reducer: `function reduce(s,e,c){ if(e.type==='@init'){ throw new Error('boom'); } return s; }`})
	viewThrow := smokeReducer(authEndpoint{path: "g",
		reducer: `function reduce(s,e,c){ return {x:1}; } function view(s,p){ throw new Error('leak'); }`})
	if initThrow == nil || viewThrow == nil {
		t.Fatal("expected both broken reducers to be flagged")
	}
	if initThrow.Signature == "" || viewThrow.Signature == "" {
		t.Fatal("a reducer-smoke issue must carry a non-empty Signature")
	}
	if !strings.HasPrefix(initThrow.Signature, "reducer:") {
		t.Errorf("reducer Signature should be namespaced under reducer:, got %q", initThrow.Signature)
	}
	if initThrow.Signature == viewThrow.Signature {
		t.Fatalf("distinct reducer phases must carry distinct Signatures, got %q for both", initThrow.Signature)
	}
	if initThrow.Fingerprint() == viewThrow.Fingerprint() {
		t.Fatal("distinct reducer phases on one endpoint must not share a fingerprint")
	}
}

// The room actor drops durable writes declared on @tick, so a reducer that persists
// from its tick branch believes it saves and never does. set_reducer surfaces that as
// a warning built from the same probe run.
func TestReducerTickWriteWarning(t *testing.T) {
	writing := `function reduce(s, e, c) {
  if (e.type === '@init') { return { x: 0 }; }
  if (e.type === '@tick') { s.x += e.dt; c.update('players', { score: { '$increment': 1 } }, { id: 1 }); return s; }
  return s;
}`
	if w := ReducerTickWriteWarning("g", writing, 30); !strings.Contains(w, "@tick") {
		t.Errorf("a reducer writing on @tick produced no warning: %q", w)
	}

	clean := `function reduce(s, e, c) {
  if (e.type === '@init') { return { x: 0 }; }
  if (e.type === '@tick') { s.x += e.dt; return s; }
  if (e.type === 'win') { c.update('players', { score: { '$increment': 1 } }, { id: 1 }); return s; }
  return s;
}`
	if w := ReducerTickWriteWarning("g", clean, 30); w != "" {
		t.Errorf("a reducer writing only on actions was flagged: %q", w)
	}

	// Without a tick rate @tick is never delivered, so there is nothing to warn about.
	if w := ReducerTickWriteWarning("g", writing, 0); w != "" {
		t.Errorf("tick_hz 0 must not warn: %q", w)
	}
}
