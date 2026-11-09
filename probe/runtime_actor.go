package probe

// Authoritative-reducer smoke pass. set_reducer confirms a reducer PARSES but
// never RUNS it; this pass runs each authoritative endpoint's reducer through
// the platform lifecycle in the same goja sandbox the room actor uses. Pure
// in-process, no browser, so it runs in the static tier even without Chrome.
//
// SCOPE: only the events the PLATFORM sends (@init, @join, @tick, @timer, view)
// are driven - a throw on one is a bug. It deliberately does NOT synthesize
// player actions: their payload shape is the app's alone, so an invented one
// would fault a correct reducer. The static source checks (reducerArity,
// reducerUnboundRefs) cover the lines the run never drives.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/roomactor"
)

type authEndpoint struct {
	path    string
	reducer string
	tickHz  int
}

// probeSimSeconds is how much simulated time the tick pass covers. Long enough to
// walk a reducer through the transitions its own timers schedule (a countdown
// expiring into play is where a sim reducer typically first faults), short enough
// to stay inside the static tier's budget.
const probeSimSeconds = 12

// RunAuthoritativeReducerSmoke compiles and lifecycle-tests every authoritative
// endpoint's reducer. Returns one error issue per broken reducer; no-op when
// there are none.
func RunAuthoritativeReducerSmoke(ctx context.Context, db *sql.DB) []ValidationIssue {
	if db == nil {
		return nil
	}
	// model='authoritative' catches an endpoint created with manage_authoritative whose
	// set_reducer never ran: authoritative=1 is set only by set_reducer, so that endpoint
	// would otherwise ship as a trusting relay with no build finding at all.
	rows, err := db.Query("SELECT path, COALESCE(reducer,''), COALESCE(tick_hz,0) FROM of_ws_endpoints WHERE COALESCE(authoritative,0) = 1 OR COALESCE(model,'') = 'authoritative'")
	if err != nil {
		// A project DB predating the model column: fall back to the flag alone.
		rows, err = db.Query("SELECT path, COALESCE(reducer,''), COALESCE(tick_hz,0) FROM of_ws_endpoints WHERE COALESCE(authoritative,0) = 1")
		if err != nil {
			return nil
		}
	}
	var eps []authEndpoint
	for rows.Next() {
		var e authEndpoint
		if rows.Scan(&e.path, &e.reducer, &e.tickHz) == nil && e.path != "" {
			eps = append(eps, e)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return nil
	}

	var issues []ValidationIssue
	for _, e := range eps {
		if ctx.Err() != nil {
			break
		}
		// Static source checks first: both name the exact line, while the run's throw
		// names only the event, and both hold on lines the run never drives. Arity
		// leads because an over-applied call is usually the CAUSE of the throw the run
		// then reports.
		if iss := reducerArity(e); iss != nil {
			issues = append(issues, *iss)
		}
		if iss := reducerUnboundRefs(e); iss != nil {
			issues = append(issues, *iss)
		}
		if iss := smokeReducer(e); iss != nil {
			issues = append(issues, *iss)
		}
	}
	return issues
}

// CheckReducerSource runs the build-time reducer checks (arity, unbound references,
// the @init/@join/@tick/@timer/view lifecycle run) against one reducer BEFORE it is
// stored, so manage_authoritative set_reducer reports a throw at attach time instead
// of the model learning about it at build end. No issues = the reducer passes.
func CheckReducerSource(path, code string, tickHz int) []ValidationIssue {
	e := authEndpoint{path: path, reducer: code, tickHz: tickHz}
	var issues []ValidationIssue
	for _, check := range []func(authEndpoint) *ValidationIssue{reducerArity, reducerUnboundRefs, smokeReducer} {
		if iss := check(e); iss != nil {
			issues = append(issues, *iss)
		}
	}
	return issues
}

// reducerFixSteer is the single home for how the model repairs a reducer this pass
// faulted. Both findings on an endpoint end with it, so the instruction cannot come
// out two different ways for one file.
func reducerFixSteer(path string) string {
	return fmt.Sprintf("Read the current reducer with manage_authoritative action=get endpoint_path=%s, fix the offending line, and re-attach with action=set_reducer (do not blind-rewrite it).", path)
}

// reducerArity reports same-function calls given more arguments than the
// function declares. Static, so it holds on lines the run never drives. Its own
// Category and Signature keep it from sharing a Fingerprint() with the lifecycle
// finding on the same endpoint: a shared fingerprint would let one demote the
// other in the fix loop.
func reducerArity(e authEndpoint) *ValidationIssue {
	if e.reducer == "" {
		return nil // the missing-reducer case is the run's to report, not this one's
	}
	name := "reducer:" + e.path
	mism := analysis.ArityOverApplications(name, e.reducer)
	if len(mism) == 0 {
		return nil
	}
	return &ValidationIssue{
		Severity:      "warning", // see the static arity_mismatch: usually an ignored extra argument
		Category:      "arity_mismatch",
		Page:          "/api/" + e.path + "/ws",
		Deterministic: true,
		Signature:     "reducer-arity",
		Message:       analysis.ExplainArityMismatches(name, mism) + " " + reducerFixSteer(e.path),
	}
}

// reducerUnboundRefs reports identifiers the reducer uses in a member-base or
// assignment position that NO enclosing scope binds - a guaranteed
// `<name> is not defined` throw. Static, so it holds on lines the run never
// drives. Severity is a FLAT deterministic error: a reducer runs in a bare goja
// sandbox, so an unbound ref can only be a throw. Gated on a successful
// compile - scoping invalid JS could invent an unbound name, which this
// analysis must never do.
func reducerUnboundRefs(e authEndpoint) *ValidationIssue {
	if e.reducer == "" {
		return nil // the missing-reducer case is the run's to report, not this one's
	}
	if _, err := roomactor.Compile(e.reducer); err != nil {
		return nil // an unparseable reducer is the run's compile finding; don't scope-scan invalid JS
	}
	var unbound []analysis.UndefinedRef
	for _, r := range analysis.UndefinedRefs("reducer:"+e.path, e.reducer) {
		if r.Kind == analysis.RefMember || r.Kind == analysis.RefAssign {
			unbound = append(unbound, r)
		}
	}
	if len(unbound) == 0 {
		return nil
	}
	return &ValidationIssue{
		Severity:      "error",
		Category:      "undefined_reference",
		Page:          "/api/" + e.path + "/ws",
		Deterministic: true,
		// Its own Category and Signature keep it from sharing a Fingerprint() with the
		// arity finding (arity_mismatch/reducer-arity) or the lifecycle finding
		// (websocket/reducer:<phase>) on the same endpoint: three distinct defects, and
		// a shared fingerprint would let one demote the others in the fix loop.
		Signature: "reducer-unbound",
		Message:   explainReducerUnboundRefs(e.path, unbound) + " " + reducerFixSteer(e.path),
	}
}

// explainReducerUnboundRefs names each unbound identifier and its line, steering
// the reducer-appropriate fix (thread the parameter). It does NOT reuse
// analysis.ExplainUndefinedRef, whose "import it" steer fits modules, not a
// single sandboxed body with nothing to import from.
func explainReducerUnboundRefs(path string, refs []analysis.UndefinedRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Authoritative WebSocket endpoint %q has a broken reducer: it references", path)
	for i, r := range refs {
		switch {
		case i == 0:
			b.WriteString(" ")
		case i == len(refs)-1:
			b.WriteString(" and ")
		default:
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "`%s` (line %d)", r.Name, r.Line)
	}
	b.WriteString(" that no enclosing scope binds, so `<name> is not defined` throws the instant that line runs and freezes the room. reduce(state, event, ctx) hands state, event, and ctx to the function as PARAMETERS, not globals: ctx.random()/ctx.now() are methods on that ctx argument, not ambient the way the Math.random()/Date.now() they replace are. A helper factored out of reduce (a resetBall, a startRound) must TAKE the value it uses as a parameter and every caller must PASS it - resetBall(state, ctx, dir), not resetBall(state, dir).")
	return b.String()
}

// ReducerTickWriteWarning runs the same lifecycle as the attach-time smoke and reports
// when the reducer declares durable writes (ctx.insert/update/upsert) while handling
// @tick. The room actor drops those on purpose (a sim ticks tens of times a second, so
// persisting each would be a write storm), which makes them a silent no-op. Empty
// means none were seen, or the reducer did not run clean (the smoke reports that).
// Best effort: only the @tick events of the probe run are observed.
func ReducerTickWriteWarning(path, code string, tickHz int) string {
	if tickHz <= 0 {
		return ""
	}
	iss, out := smokeReducerRun(authEndpoint{path: path, reducer: code, tickHz: tickHz})
	if iss != nil || out.tickWrites == 0 {
		return ""
	}
	return fmt.Sprintf("the reducer declares ctx.insert/update/upsert while handling @tick (%d such call(s) in the probe run). The room actor DROPS writes declared on @tick, with no error, so they never reach the database. Move durable writes to an action, @join/@leave, or an @timer branch (a ctx.after timer), and keep @tick for advancing the in-memory state.", out.tickWrites)
}

func smokeReducer(e authEndpoint) *ValidationIssue {
	iss, _ := smokeReducerRun(e)
	return iss
}

// smokeReducerRun is smokeReducer plus what the simulated run observed.
func smokeReducerRun(e authEndpoint) (*ValidationIssue, simOutcome) {
	var out simOutcome
	iss := smokeReducerBody(e, &out)
	return iss, out
}

func smokeReducerBody(e authEndpoint, outp *simOutcome) *ValidationIssue {
	fix := func(phase, detail string) *ValidationIssue {
		return &ValidationIssue{
			Severity:      "error",
			Category:      "websocket",
			Page:          "/api/" + e.path + "/ws",
			Deterministic: true,
			// Signature namespaces this finding away from the runtime_ws welcome-smoke
			// finding on the SAME endpoint, which would otherwise collapse to one
			// Fingerprint() and demote each other in the fix loop. The phase keeps a
			// newly-revealed lifecycle failure reading as new.
			Signature: "reducer:" + phase,
			Message: fmt.Sprintf("Authoritative WebSocket endpoint %q has a broken reducer: %s. %s",
				e.path, detail, reducerFixSteer(e.path)),
		}
	}

	if e.reducer == "" {
		return fix("missing", "the endpoint is authoritative but has no reducer attached")
	}
	red, err := roomactor.Compile(e.reducer)
	if err != nil {
		return fix("compile", "it does not compile: "+err.Error())
	}

	p1 := roomactor.Player{ID: "probe-p1", Meta: json.RawMessage(`{"name":"P1"}`)}
	p2 := roomactor.Player{ID: "probe-p2", Meta: json.RawMessage(`{"name":"P2"}`)}

	// @init (no players yet), then two @join, mirroring the actor's spawn + first
	// two arrivals. Timers accumulate across all three because the actor applies
	// them after EVERY successful reduce; dropping one would leave the probe
	// simulating a room the server does not run.
	var pending []roomactor.TimerOp
	res, err := red.Reduce(nil, map[string]any{"type": "@init"}, nil, 1, 0)
	if iss := classify(fix, "@init", err); iss != nil {
		return iss
	}
	state := res.State
	pending = append(pending, res.Timers...)

	res, err = red.Reduce(state, map[string]any{"type": "@join", "player": map[string]any{"id": p1.ID, "meta": p1.Meta}}, []roomactor.Player{p1}, 2, 0)
	if iss := classify(fix, "@join", err); iss != nil {
		return iss
	}
	state = res.State
	pending = append(pending, res.Timers...)

	res, err = red.Reduce(state, map[string]any{"type": "@join", "player": map[string]any{"id": p2.ID, "meta": p2.Meta}}, []roomactor.Player{p1, p2}, 3, 0)
	if iss := classify(fix, "@join", err); iss != nil {
		return iss
	}
	state = res.State
	pending = append(pending, res.Timers...)

	// Run the room. Everything above is the lobby; a game's logic lives in the
	// events that come after the players are seated, so a pass that stops here
	// certifies the half of the reducer that was never in doubt.
	state, out, iss := simulate(fix, red, state, pending, e.tickHz, []roomactor.Player{p1, p2})
	*outp = out
	if iss != nil {
		return iss
	}

	// view() must project for each player without throwing/looping (a leak it
	// can't prove without a schema, but a crashing/looping view is a hard break).
	if red.HasView() {
		if _, err := red.View(state, p1.ID); err != nil {
			return classifyView(fix, err)
		}
		if _, err := red.View(state, p2.ID); err != nil {
			return classifyView(fix, err)
		}
	}

	// Last, because a fault is a harder break than a stall and should be the one
	// the model is handed first.
	return stalled(fix, e, out)
}

// simOutcome is what the run observed BEYOND whether anything faulted: enough to
// tell a room that is still advancing from one that has stopped and cannot start
// again.
type simOutcome struct {
	moved     bool // the state changed at least once under @tick/@timer
	movedLate bool // ...and changed again during the LAST half of the run
	armed     int  // timers still pending when the run ended
	// tickWrites counts @tick events that declared durable writes. The room actor drops
	// those, so a nonzero count is a reducer that believes it persists and does not.
	tickWrites int
}

// stalled reports a room that advanced, stopped, and has nothing left that could
// restart it. Deliberately NOT a general freeze check - whether a room SHOULD be
// advancing is the app's business, and a probe that guessed would block good
// builds. Every clause is load-bearing: tick_hz > 0 (the endpoint exists to
// advance a sim), moved (the room STARTED; a waiting lobby never moves and is
// never flagged), !movedLate (byte-identical state for the whole back half),
// armed == 0 (nothing scheduled that could resume it).
func stalled(fix func(phase, detail string) *ValidationIssue, e authEndpoint, out simOutcome) *ValidationIssue {
	if e.tickHz <= 0 || !out.moved || out.movedLate || out.armed > 0 {
		return nil
	}
	return fix("inert", fmt.Sprintf(
		"it runs clean but the room STOPS: after advancing, the last %d seconds of simulated play left the state byte-identical at %dHz with no timer pending, so nothing can move it again. This is the shape of a countdown or phase that never completes - typically a ctx.after timer whose @timer branch decrements but never re-arms, or a phase that waits for a timer that already fired. Re-arm it from its own @timer branch, or count down in the @tick branch using event.dt (seconds), which cannot stall because @tick always comes",
		probeSimSeconds/2, e.tickHz))
}

// simulate advances the room over probeSimSeconds of virtual time, mirroring the
// actor: @tick at the endpoint's rate, @timer when an armed one comes due, no
// state commit when an event faults. The clock is virtual so the pass is cheap
// and deterministic. A non-ticking endpoint still runs: turn-based rooms
// schedule timers, and a reducer that faults on its own turn timer is broken.
func simulate(fix func(phase, detail string) *ValidationIssue, red *roomactor.Reducer,
	state json.RawMessage, pending []roomactor.TimerOp, tickHz int, players []roomactor.Player) (json.RawMessage, simOutcome, *ValidationIssue) {

	hz := tickHz
	if hz <= 0 {
		hz = 10 // no sim to advance; a rate just paces the timer clock
	}
	dt := 1.0 / float64(hz)

	timers := map[string]float64{}
	arm := func(now float64, ops []roomactor.TimerOp) {
		for _, op := range ops {
			if op.Set {
				timers[op.Name] = now + float64(op.MS)/1000
			} else {
				delete(timers, op.Name)
			}
		}
	}
	arm(0, pending)

	var out simOutcome
	total := probeSimSeconds * hz
	evCount := uint64(3)
	// Compared as bytes because that is the whole claim: the reducer returned a
	// state indistinguishable from the one it was handed, so the room did nothing.
	step := func(i int, now float64, phase string, event map[string]any) *ValidationIssue {
		evCount++
		res, err := red.Reduce(state, event, players, evCount, int64(now*1000))
		if err != nil {
			return classify(fix, phase, err)
		}
		if phase == "@tick" && len(res.Writes) > 0 {
			out.tickWrites++
		}
		if !bytes.Equal(state, res.State) {
			out.moved = true
			if i >= total/2 {
				out.movedLate = true
			}
		}
		state = res.State
		arm(now, res.Timers)
		return nil
	}

	for i := 0; i < total; i++ {
		now := float64(i) * dt
		// Timers first: the actor delivers a due @timer as its own event, and a
		// reducer that re-arms from that branch depends on seeing it before the
		// tick that would otherwise carry the state past it.
		for _, name := range dueTimers(timers, now) {
			delete(timers, name)
			if iss := step(i, now, "@timer", map[string]any{"type": "@timer", "name": name}); iss != nil {
				return state, out, iss
			}
		}
		if tickHz > 0 {
			if iss := step(i, now, "@tick", map[string]any{"type": "@tick", "dt": dt}); iss != nil {
				return state, out, iss
			}
		}
	}
	out.armed = len(timers)
	return state, out, nil
}

// dueTimers returns the names due at now, sorted, so a reducer arming several
// cannot make the pass order-dependent (and therefore flaky).
func dueTimers(timers map[string]float64, now float64) []string {
	var due []string
	for name, at := range timers {
		if at <= now {
			due = append(due, name)
		}
	}
	sort.Strings(due)
	return due
}

// classify turns a reduce error on a platform-sent event into an issue. A
// rejection of one is itself a bug (the platform, not a player, sends these).
// The phase is both named in the message and folded into the issue Signature, so
// a fix that clears one event and reveals the next earns its own pass.
func classify(fix func(phase, detail string) *ValidationIssue, phase string, err error) *ValidationIssue {
	if err == nil {
		return nil
	}
	var to *roomactor.Timeout
	var rej *roomactor.Rejection
	switch {
	case errors.As(err, &to):
		return fix(phase, fmt.Sprintf("it timed out on the %s event (likely an infinite loop)", phase))
	case errors.As(err, &rej):
		return fix(phase, fmt.Sprintf("it rejected the %s event (%q) - the platform sends this one, not a player, so reduce must handle it rather than reject it", phase, rej.Msg))
	default:
		return fix(phase, fmt.Sprintf("it threw on the %s event: %v", phase, err))
	}
}

func classifyView(fix func(phase, detail string) *ValidationIssue, err error) *ValidationIssue {
	var to *roomactor.Timeout
	if errors.As(err, &to) {
		return fix("view", "its view() timed out (likely an infinite loop)")
	}
	return fix("view", fmt.Sprintf("its view() threw: %v", err))
}
