// Package roomactor holds the sandboxed, model-authored room reducer for the
// authoritative realtime lane, deliberately free of any hub / DB / network
// dependency so the only things a reducer can touch are the values injected here.
// A room actor owns ONE Reducer and calls it from a single goroutine; Reducer is
// NOT safe for concurrent use.
package roomactor

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

// Budget is the wall-clock cap on a single reduce/view call. A reducer that
// exceeds it is interrupted, so a runaway loop fails one event instead of hanging
// the room or the server.
const Budget = 50 * time.Millisecond

// CompileBudget is the wall-clock cap on running the script's top-level code in
// Compile. Compile runs on every room join and at save time, so a top-level
// runaway loop must fail the compile instead of pinning a goroutine forever.
const CompileBudget = time.Second

// MaxStateBytes caps the serialized authoritative state. goja has no hard heap
// cap, so bounding state size (plus the inbound message cap) is how memory stays
// bounded.
const MaxStateBytes = 256 * 1024

// Player is a connected participant as the reducer sees it. UID is the
// server-stamped authenticated user id (0 when anonymous), set by the actor from
// the JWT and never from client-supplied meta, so a reducer can key durable
// reads/writes on a tamper-proof identity.
type Player struct {
	ID   string          `json:"id"`
	UID  int             `json:"uid,omitempty"`
	Meta json.RawMessage `json:"meta,omitempty"`
}

// TimerOp is a per-room timer request the reducer made via ctx.after /
// ctx.cancel; the actor schedules/cancels them server-side. MS is the internal
// wire unit; the reducer API takes seconds, matching @tick's dt.
type TimerOp struct {
	Name string
	Set  bool // true = after(seconds), false = cancel
	MS   int
}

// Write is a durable DB write the reducer declared via ctx.insert / ctx.update /
// ctx.upsert - a DECLARATION, not a side effect: the reducer stays pure and the
// actor performs the write after a successful reduce. Row may carry
// {"$increment": n} / {"$decrement": n}; Conflict names the upsert's conflict-key columns.
type Write struct {
	Op       string
	Table    string
	Row      map[string]any
	Where    map[string]any
	Conflict []string
}

// MaxWrites caps DB writes declared in a single reduce. A reducer that exceeds it
// fails that one event (like the state-size cap) rather than flooding the DB.
const MaxWrites = 32

// Result is the outcome of a successful reduce.
type Result struct {
	State  json.RawMessage
	Timers []TimerOp
	Writes []Write
}

// Rejection is returned when the reducer calls ctx.reject(msg): a clean refusal
// of the action (no state change), surfaced only to the sender.
type Rejection struct{ Msg string }

func (e *Rejection) Error() string { return e.Msg }

// Timeout is returned when a reduce/view exceeds Budget.
type Timeout struct{}

func (e *Timeout) Error() string { return "reducer exceeded time budget" }

// rejectSignal is panicked by ctx.reject and recovered in Reduce. goja re-panics
// unknown Go panic values out of the native call, so this reaches our recover.
type rejectSignal struct{ msg string }

// Reducer is a compiled reduce()/optional view() pair in a locked-down runtime.
type Reducer struct {
	vm        *goja.Runtime
	reduce    goja.Callable
	view      goja.Callable // nil when the script defines no view()
	hasView   bool
	jsonParse goja.Callable // native JSON.parse, captured before user code runs
}

// Compile parses reducer source and binds reduce()/view() at set_reducer save
// time AND when an actor spawns, so "valid on save" == "valid at runtime".
// sandboxInit strips ambient nondeterminism from the locked-down goja runtime so
// a reducer must use the injected ctx: Math.random and Date.now throw; the Date
// constructor throws ONLY on the zero-arg form - new Date(ms) / Date.parse /
// Date.UTC stay available and deterministic.
const sandboxInit = `(function(){
  if (typeof Math !== 'undefined') { Math.random = function(){ throw new Error('use ctx.random()'); }; }
  if (typeof Date !== 'undefined') {
    var _OD = Date;
    function _GD(a,b,c,d,e,f,g){
      if (arguments.length === 0) { throw new Error('use ctx.now() - new Date() is non-deterministic'); }
      switch (arguments.length) {
        case 1: return new _OD(a);
        case 2: return new _OD(a,b);
        case 3: return new _OD(a,b,c);
        case 4: return new _OD(a,b,c,d);
        case 5: return new _OD(a,b,c,d,e);
        case 6: return new _OD(a,b,c,d,e,f);
        default: return new _OD(a,b,c,d,e,f,g);
      }
    }
    _GD.prototype = _OD.prototype;
    _GD.parse = _OD.parse;
    _GD.UTC = _OD.UTC;
    _GD.now = function(){ throw new Error('use ctx.now()'); };
    Date = _GD;
  }
})();`

func Compile(code string) (*Reducer, error) {
	vm := goja.New()
	// Bound the JS call stack so a runaway recursion returns a catchable error
	// instead of growing the Go stack until the process fatals (uncatchable, and it
	// would take down every project in this single binary).
	vm.SetMaxCallStackSize(1000)
	if _, err := vm.RunString(sandboxInit); err != nil {
		return nil, fmt.Errorf("sandbox init: %w", err)
	}
	// Capture the native JSON.parse before user code runs, so state is always
	// deserialized with the built-in parser regardless of what the reducer defines.
	jsonParse := captureJSONParse(vm)
	stop := time.AfterFunc(CompileBudget, func() { vm.Interrupt("timeout") })
	_, err := vm.RunString(code)
	stop.Stop()
	vm.ClearInterrupt()
	if err != nil {
		var ie *goja.InterruptedError
		if errors.As(err, &ie) {
			return nil, fmt.Errorf("reducer top-level code exceeded %s; move work into reduce()", CompileBudget)
		}
		return nil, fmt.Errorf("reducer does not parse: %w", err)
	}
	reduceFn, ok := goja.AssertFunction(vm.Get("reduce"))
	if !ok {
		return nil, errors.New("reducer must define a function reduce(state, event, ctx)")
	}
	r := &Reducer{vm: vm, reduce: reduceFn, jsonParse: jsonParse}
	if vv := vm.Get("view"); vv != nil {
		if fn, ok := goja.AssertFunction(vv); ok {
			r.view = fn
			r.hasView = true
		}
	}
	return r, nil
}

// HasView reports whether the script defined a per-player view() projection.
func (r *Reducer) HasView() bool { return r.hasView }

// Reduce runs the authoritative transition for one event. seed drives ctx.random
// (the actor advances it per event for determinism); nowMS is the injected clock.
// Returns *Rejection on ctx.reject, *Timeout on budget overrun, a plain error on
// a thrown exception or a non-serializable / oversized result.
func (r *Reducer) Reduce(state json.RawMessage, event map[string]any, players []Player, seed uint64, nowMS int64) (res *Result, err error) {
	rng := &prng{s: seed}
	var timers []TimerOp
	var writes []Write
	rejected := false
	rejectMsg := ""

	ctx := r.vm.NewObject()
	_ = ctx.Set("random", func() float64 { return rng.next() })
	_ = ctx.Set("now", func() int64 { return nowMS })
	_ = ctx.Set("players", r.toJSValue(playersJSON(players)))
	_ = ctx.Set("reject", func(msg string) { rejected = true; rejectMsg = msg; panic(rejectSignal{msg}) })
	// ctx.after(name, seconds) fires ONE @timer{name} after the delay, and the name
	// says so: a repeating schedule is the caller re-arming from the @timer branch.
	// Seconds, not milliseconds, so it reads in the same unit as @tick's dt and a
	// countdown can be spelled either way without a conversion in between.
	_ = ctx.Set("after", func(name string, seconds float64) {
		// Clamp the delay from model code: a NaN or negative value would fire the
		// timer immediately, and an absurdly large one overflows the int millisecond
		// conversion (and the downstream time.Duration). Floor at zero (NaN included,
		// since NaN > 0 is false) and cap at a day - rooms are short-lived.
		if !(seconds > 0) {
			seconds = 0
		} else if seconds > 86400 {
			seconds = 86400
		}
		timers = append(timers, TimerOp{Name: name, Set: true, MS: int(seconds * 1000)})
	})
	_ = ctx.Set("cancel", func(name string) { timers = append(timers, TimerOp{Name: name, Set: false}) })
	// ctx.insert(table, row) / ctx.update(table, patch, where) / ctx.upsert(table,
	// row, conflictColumns): DECLARE a durable write. The actor performs it after
	// this reduce succeeds (never here, the sandbox holds no DB handle and this
	// call is on the reduce time budget).
	_ = ctx.Set("insert", func(table string, row map[string]any) {
		writes = append(writes, Write{Op: "insert", Table: table, Row: row})
	})
	_ = ctx.Set("update", func(table string, patch, where map[string]any) {
		writes = append(writes, Write{Op: "update", Table: table, Row: patch, Where: where})
	})
	// upsert is the one safe primitive for keyed cell/grid/voxel state whose key may
	// be NEW or existing: insert the row, or on a UNIQUE/PK conflict over
	// conflictColumns update the existing row to it. ctx.update alone silently no-ops
	// (does NOT insert) when the row isn't there yet, dropping every brand-new edit.
	_ = ctx.Set("upsert", func(table string, row map[string]any, conflict []string) {
		writes = append(writes, Write{Op: "upsert", Table: table, Row: row, Conflict: conflict})
	})

	// Recover the reject sentinel (goja re-panics unknown Go panics out of the
	// native call). Any other panic is re-raised. The rejected flag below is the
	// belt-and-suspenders path if a goja version turns the panic into a thrown
	// exception instead.
	defer func() {
		if rec := recover(); rec != nil {
			if rs, ok := rec.(rejectSignal); ok {
				res, err = nil, &Rejection{Msg: rs.msg}
				return
			}
			// Any OTHER panic (a goja host panic, an unexpected native error) must
			// NOT escape: re-raising would unwind past the actor's handle() and kill
			// the room goroutine, freezing the room for every client. Surface it as a
			// failed event so the actor rejects just this transition and keeps running.
			res, err = nil, fmt.Errorf("reduce panicked: %v", rec)
			return
		}
	}()

	stop := time.AfterFunc(Budget, func() { r.vm.Interrupt("timeout") })
	defer stop.Stop()
	defer r.vm.ClearInterrupt()

	ret, gerr := r.reduce(goja.Undefined(), r.toJSValue(state), r.vm.ToValue(event), ctx)
	if rejected {
		return nil, &Rejection{Msg: rejectMsg}
	}
	if gerr != nil {
		var ie *goja.InterruptedError
		if errors.As(gerr, &ie) {
			return nil, &Timeout{}
		}
		return nil, fmt.Errorf("reduce threw: %w", gerr)
	}
	newState, serr := exportJSON(ret)
	if serr != nil {
		return nil, fmt.Errorf("reduce returned a non-JSON state: %w", serr)
	}
	if len(newState) > MaxStateBytes {
		return nil, fmt.Errorf("reduce returned %d bytes of state (limit %d)", len(newState), MaxStateBytes)
	}
	if len(writes) > MaxWrites {
		return nil, fmt.Errorf("reduce declared %d DB writes (limit %d)", len(writes), MaxWrites)
	}
	return &Result{State: newState, Timers: timers, Writes: writes}, nil
}

// View projects the authoritative state to what playerID is allowed to see; with
// no view() defined the caller broadcasts the full state. No clock is injected:
// a time-dependent view would render differently for two players on one rev. A
// view that needs the time reads it from a field reduce() put there.
func (r *Reducer) View(state json.RawMessage, playerID string) (out json.RawMessage, err error) {
	if !r.hasView {
		return state, nil
	}
	stop := time.AfterFunc(Budget, func() { r.vm.Interrupt("timeout") })
	defer stop.Stop()
	defer r.vm.ClearInterrupt()

	ret, gerr := r.view(goja.Undefined(), r.toJSValue(state), r.vm.ToValue(playerID))
	if gerr != nil {
		var ie *goja.InterruptedError
		if errors.As(gerr, &ie) {
			return nil, &Timeout{}
		}
		return nil, fmt.Errorf("view threw: %w", gerr)
	}
	v, serr := exportJSON(ret)
	if serr != nil {
		return nil, fmt.Errorf("view returned a non-JSON value: %w", serr)
	}
	return v, nil
}

// --- helpers ---------------------------------------------------------------

// captureJSONParse returns the runtime's built-in JSON.parse, or nil if it is
// unavailable. Called once at compile time so toJSValue always reaches the native
// parser.
func captureJSONParse(vm *goja.Runtime) goja.Callable {
	jsonV := vm.Get("JSON")
	if jsonV == nil || goja.IsUndefined(jsonV) || goja.IsNull(jsonV) {
		return nil
	}
	obj := jsonV.ToObject(vm)
	if obj == nil {
		return nil
	}
	fn, ok := goja.AssertFunction(obj.Get("parse"))
	if !ok {
		return nil
	}
	return fn
}

// toJSValue converts raw JSON state into a goja value; empty/nil -> undefined so
// the reducer can branch on `state === undefined` for @init. Deserialized with
// the native JSON.parse so in-place mutations round-trip through Export (a value
// reflected from a decoded Go slice does not grow on push); direct reflect stays
// as a fallback.
func (r *Reducer) toJSValue(raw json.RawMessage) goja.Value {
	if len(raw) == 0 || string(raw) == "null" {
		return goja.Undefined()
	}
	if r.jsonParse != nil {
		if v, err := r.jsonParse(goja.Undefined(), r.vm.ToValue(string(raw))); err == nil {
			return v
		}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return goja.Undefined()
	}
	return r.vm.ToValue(v)
}

// exportJSON serializes a goja return value to canonical JSON via Go export.
func exportJSON(v goja.Value) (json.RawMessage, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(v.Export())
	if err != nil {
		// json.Marshal rejects NaN and +/-Infinity - the usual way a reducer produces
		// an unserializable value (a divide-by-zero, a Math result gone non-finite).
		// Name it so the fault is actionable instead of a raw encoder error.
		var ue *json.UnsupportedValueError
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("holds a non-finite number (%s); reduce must not put NaN or Infinity in state - check divisions and Math results", ue.Str)
		}
		return nil, err
	}
	return b, nil
}

// playersJSON renders players (with parsed meta) to a generic value reducers read.
func playersJSON(players []Player) []byte {
	b, err := json.Marshal(players)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// prng is splitmix64, seeded per call by the actor for deterministic ctx.random.
type prng struct{ s uint64 }

func (p *prng) next() float64 {
	p.s += 0x9E3779B97F4A7C15
	z := p.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z = z ^ (z >> 31)
	return float64(z>>11) / float64(uint64(1)<<53)
}
