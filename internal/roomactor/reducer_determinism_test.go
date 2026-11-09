package roomactor

import (
	"encoding/json"
	"testing"
)

// new Date() reads the wall clock, so it is non-deterministic and would break
// replay/resume of persisted room state. The sandbox makes the zero-arg form
// throw; a reducer that calls it fails the event instead of silently diverging.
func TestReduceRejectsZeroArgDate(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,ctx){ return { t: new Date().getTime() }; }`)
	if _, err := r.Reduce(json.RawMessage(`{}`), map[string]any{"type": "go"}, nil, 1, 0); err == nil {
		t.Fatal("expected new Date() to be rejected as non-deterministic")
	}
}

// Date.now() is blocked the same way (regression for the original guard).
func TestReduceRejectsDateNow(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,ctx){ return { t: Date.now() }; }`)
	if _, err := r.Reduce(json.RawMessage(`{}`), map[string]any{"type": "go"}, nil, 1, 0); err == nil {
		t.Fatal("expected Date.now() to be rejected as non-deterministic")
	}
}

// Explicit construction stays available and deterministic, so a reducer handed an
// explicit timestamp (e.g. from ctx.now()) can still build a Date.
func TestReduceExplicitDateWorks(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,ctx){ return { t: new Date(0).getTime() }; }`)
	res, err := r.Reduce(json.RawMessage(`{}`), map[string]any{"type": "go"}, nil, 1, 0)
	if err != nil {
		t.Fatalf("explicit new Date(0) should work: %v", err)
	}
	if got := decode(t, res.State)["t"].(float64); got != 0 {
		t.Fatalf("new Date(0).getTime() = %v, want 0", got)
	}
}

// A runaway recursion must return a catchable error, NOT grow the Go stack until
// the process fatals (uncatchable - it would crash this test binary and, in prod,
// every project in the single binary). Reaching the assertion at all proves the
// process survived the overflow.
func TestReduceDeepRecursionDoesNotCrash(t *testing.T) {
	r := mustCompile(t, `function reduce(s,e,ctx){ function f(n){ return f(n+1); } return f(0); }`)
	if _, err := r.Reduce(json.RawMessage(`{}`), map[string]any{"type": "go"}, nil, 1, 0); err == nil {
		t.Fatal("expected deep recursion to fail the event, got nil error")
	}
}
