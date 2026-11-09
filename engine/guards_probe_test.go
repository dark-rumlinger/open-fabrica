package engine

import (
	"fmt"
	"testing"
)

// probeArgs is the read-only page-probe call shape: identical every turn by
// design (you load `/` the same way each time).
func probeArgs() map[string]any {
	return map[string]any{"action": "test_page", "page_path": "/"}
}

// A fix-and-reprobe trajectory: probe `/` (fails on one bug) -> fix that file ->
// probe (fails on a different bug) -> fix -> probe (fails on a third bug) -> fix ->
// probe. Each probe fails on a DIFFERENT bug. Identical probe arguments across
// intervening successful writes are not a spin, so the fourth probe must NOT be
// blocked: it is how the model confirms its last fix worked.
func TestRepeatGuard_ProbeNotBlockedAcrossInterveningWrites(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	files := []string{"main.js", "planet.js", "procedural.js"}

	for i, f := range files {
		// Probe fails on the bug currently surfaced.
		fail := fmt.Sprintf(`{"success":false,"error":"uncaught exception number %d"}`, i)
		if v := run(g, st, "manage_testing", probeArgs(), fail, false); v != nil && v.Block {
			t.Fatalf("probe %d must not be blocked", i)
		}
		// The model fixes a file (a successful state-changing write).
		if v := run(g, st, "manage_files", saveArgs(f), `{"success":true}`, true); v != nil && v.Block {
			t.Fatalf("the fix write for %s must not be blocked", f)
		}
	}

	// The fourth probe, after the last fix, must be allowed so the model can confirm.
	if v := g.Before(st, nil, "manage_testing", probeArgs()); v != nil && v.Block {
		t.Fatalf("the probe after the final fix must run to confirm, got block: %q", v.Reply)
	}
}

// A read-only probe that keeps failing with the IDENTICAL error and NOTHING
// changed in between is genuinely stuck. The identical-result block refuses it
// (same result, no progress), so a true spin is still caught - just without
// force-stopping the whole session.
func TestRepeatGuard_ProbeIdenticalSpinStillBlocked(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	fail := `{"success":false,"error":"uncaught exception"}`

	for i := 0; i < repeatedResultBlockThreshold; i++ {
		if v := run(g, st, "manage_testing", probeArgs(), fail, false); v != nil && v.Block {
			t.Fatalf("blocked too early at attempt %d", i)
		}
	}
	v := g.Before(st, nil, "manage_testing", probeArgs())
	if v == nil || !v.Block {
		t.Fatalf("a probe re-issued with the identical failure and no change must still be refused, got %v", v)
	}
}

// A read-only probe spun with NO intervening successful write but VARYING results
// (so the identical-result block resets each turn) is still caught by the
// outcome-agnostic consecutive-stall counter: same call shape, nothing else
// between, repeatedCallStallThreshold turns in a row -> force-stop.
func TestRepeatGuard_ProbeVaryingSpinStallStops(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()

	for i := 0; i < repeatedCallStallThreshold-1; i++ {
		fail := fmt.Sprintf(`{"success":false,"error":"exception %d"}`, i)
		if v := run(g, st, "manage_testing", probeArgs(), fail, false); v != nil && v.Block {
			t.Fatalf("blocked too early at attempt %d", i)
		}
	}
	v := g.Before(st, nil, "manage_testing", probeArgs())
	if v == nil || !v.Block || !v.Stop {
		t.Fatalf("a probe issued %d times in a row with nothing else between must force-stop, got %v", repeatedCallStallThreshold, v)
	}
}
