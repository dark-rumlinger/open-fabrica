package engine

import (
	"fmt"
	"strings"
	"testing"
)

// run drives one screened call through the guard: Before, then (only if the call
// was not blocked) After with the given result/ok. Returns Before's verdict.
func run(g RepeatGuard, st *GuardState, name string, args map[string]any, result string, ok bool) *Verdict {
	v := g.Before(st, nil, name, args)
	if v != nil && v.Block {
		return v
	}
	g.After(st, nil, name, args, result, ok)
	return v
}

func saveArgs(path string) map[string]any {
	return map[string]any{"action": "save", "path": path}
}

func patchArgs(path, anchor string) map[string]any {
	return map[string]any{"action": "patch", "path": path, "anchor": anchor}
}

// The gap this closes: the OTHER arguments vary every turn (so the exact-call
// signature never repeats and never trips the byte-identical failure block) yet
// the same file returns the IDENTICAL error each time - the model is flailing on
// one file. The identical-result counter must refuse the next attempt.
func TestRepeatGuard_BlocksIdenticalResultSameFileVaryingArgs(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"syntax_error","error":"would introduce a syntax error in flight.js"}`

	for i := 0; i < repeatedResultBlockThreshold; i++ {
		// Same path, different anchor each turn -> different exact signature; only the result repeats.
		if v := run(g, st, "manage_files", patchArgs("flight.js", fmt.Sprintf("anchor%d", i)), sameErr, false); v != nil && v.Block {
			t.Fatalf("blocked too early at attempt %d", i)
		}
	}
	v := g.Before(st, nil, "manage_files", patchArgs("flight.js", "anchorX"))
	if v == nil || !v.Block {
		t.Fatalf("expected a block after %d identical results, got %v", repeatedResultBlockThreshold, v)
	}
	if v.Stop {
		t.Errorf("the identical-result block should not force-stop the session")
	}
	if !strings.Contains(v.Reply, "repeated_result_block") || !strings.Contains(v.Reply, "flight.js") {
		t.Errorf("expected a repeated_result_block reply naming the file, got %q", v.Reply)
	}
}

// A model looping on cyclic_import while editing one file at a time: the cycle path
// (and its error text) changes each attempt, so the identical-result block never arms.
// The structural redirect must still fire and hand back the whole-graph fix, not the
// generic reorient, and must not force-stop the session.
func TestRepeatGuard_RedirectsStructuralCycleToWholeGraphFix(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	for i := 0; i < structuralCycleNudgeThreshold; i++ {
		// Changing error text each attempt, so signal (1) never arms.
		err := fmt.Sprintf(`{"success":false,"code":"cyclic_import","error":"cycle via file%d"}`, i)
		if v := run(g, st, "manage_files", saveArgs("call.js"), err, false); v != nil && v.Block {
			t.Fatalf("blocked too early at attempt %d", i)
		}
	}
	v := g.Before(st, nil, "manage_files", saveArgs("call.js"))
	if v == nil || !v.Block {
		t.Fatalf("expected a structural-cycle redirect after %d cyclic_import failures, got %v", structuralCycleNudgeThreshold, v)
	}
	if v.Stop {
		t.Errorf("the structural-cycle redirect should not force-stop the session")
	}
	if !strings.Contains(v.Reply, "cyclic_import_reorient") || !strings.Contains(v.Reply, "zero-import store module") {
		t.Errorf("expected the whole-graph directive, got %q", v.Reply)
	}
}

// A non-structural reject that repeats identically hits the ordinary identical-result
// block, NOT the structural redirect - the redirect is reserved for import-cycle codes.
func TestRepeatGuard_StructuralRedirectIgnoresNonCycleCodes(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"syntax_error","error":"same error"}`
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		run(g, st, "manage_files", saveArgs("call.js"), sameErr, false)
	}
	v := g.Before(st, nil, "manage_files", saveArgs("call.js"))
	if v == nil || !v.Block {
		t.Fatalf("expected a block, got %v", v)
	}
	if strings.Contains(v.Reply, "cyclic_import_reorient") {
		t.Errorf("a syntax_error loop must not draw the structural-cycle redirect, got %q", v.Reply)
	}
}

// The identical-result block refuses its target for as long as the target stays
// stuck - a blocked call never runs After, so failedResult stays frozen at the
// threshold - but it must never force-stop the session. One target the model
// cannot satisfy leaves every other target in the plan still worth building.
func TestRepeatGuard_IdenticalResultBlockPersistsWithoutStopping(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"validation_error","error":"same failure every time"}`
	// Vary a non-target field so the exact-call signature never repeats (only the
	// identical-result signal applies) while the key stays stable across turns.
	create := func(body string) map[string]any {
		return map[string]any{"action": "create", "endpoint_path": "items", "body": body}
	}
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		if v := run(g, st, "manage_api", create(fmt.Sprintf("b%d", i)), sameErr, false); v != nil && v.Block {
			t.Fatalf("blocked too early at attempt %d", i)
		}
	}
	for i, args := range []map[string]any{create("bA"), create("bB"), create("bC")} {
		v := g.Before(st, nil, "manage_api", args)
		if v == nil || !v.Block {
			t.Fatalf("refusal %d: expected a block, got %v", i, v)
		}
		if v.Stop {
			t.Fatalf("refusal %d: a per-target block must never force-stop the session", i)
		}
		if !strings.Contains(v.Reply, "repeated_result_block") {
			t.Errorf("refusal %d: expected repeated_result_block, got %q", i, v.Reply)
		}
	}
}

// A changed result still clears the streak: the block only bites a genuinely stuck
// target, never a repair that starts converging.
func TestRepeatGuard_ResultBlockClearedByProgress(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"syntax_error","error":"same"}`
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		run(g, st, "manage_files", patchArgs("f.js", fmt.Sprintf("a%d", i)), sameErr, false)
	}
	if v := g.Before(st, nil, "manage_files", patchArgs("f.js", "x")); v == nil || !v.Block || v.Stop {
		t.Fatalf("expected a non-stopping block, got %v", v)
	}
	run(g, st, "manage_files", map[string]any{"action": "save", "path": "f.js"}, `{"success":true}`, true)
	// Progress cleared the streak; a later identical-error run starts from zero.
	if v := g.Before(st, nil, "manage_files", patchArgs("f.js", "y")); v != nil && v.Block {
		t.Fatalf("a successful write must clear the result-block streak, got %v", v)
	}
}

// Incremental repair must NOT be blocked: the same file fails with a DIFFERENT
// error each turn (the model fixes one problem and surfaces the next) and then
// LANDS. A changed result resets the identical-result streak, and a realistic
// number of distinct steps stays under the distinct-failure backstop, so genuine
// repair that converges is never refused.
func TestRepeatGuard_DoesNotBlockWhenResultChanges(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	for i := 0; i < repeatedFailureNudgeThreshold-1; i++ {
		distinctErr := fmt.Sprintf(`{"success":false,"code":"unresolved_import","error":"missing import number %d"}`, i)
		if v := run(g, st, "manage_files", patchArgs("f.js", fmt.Sprintf("a%d", i)), distinctErr, false); v != nil && v.Block {
			t.Fatalf("must not block incremental repair (distinct errors) at attempt %d", i)
		}
	}
	// The repair lands, then more distinct failures resume from zero - still fine.
	run(g, st, "manage_files", patchArgs("f.js", "ok"), `{"success":true}`, true)
	if v := g.Before(st, nil, "manage_files", patchArgs("f.js", "next")); v != nil && v.Block {
		t.Fatalf("a landed repair must reset the distinct-failure streak, got %v", v)
	}
}

// Distinct-but-wrong flailing IS caught: the same file keeps failing with a new
// error every turn (never the same, so the identical-result block never fires),
// but the model is guessing, not converging. It draws a re-orientation nudge at
// the threshold, and once the nudges are spent the target is refused for good -
// without stopping the session, which the other targets still need.
func TestRepeatGuard_ReorientsThenRefusesOnDistinctFailures(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	distinct := func(seq string) {
		err := fmt.Sprintf(`{"success":false,"code":"undefined_reference","error":"different miss %s"}`, seq)
		run(g, st, "manage_files", patchArgs("leaf.js", "anchor"+seq), err, false)
	}

	for i := 0; i < repeatedFailureNudgeThreshold; i++ {
		distinct(fmt.Sprintf("r1_%d", i))
	}
	v := g.Before(st, nil, "manage_files", patchArgs("leaf.js", "next"))
	if v == nil || !v.Block || v.Stop {
		t.Fatalf("expected a non-stopping re-orientation nudge at the threshold, got %v", v)
	}
	if !strings.Contains(v.Reply, "repeated_failure_reorient") || !strings.Contains(v.Reply, "leaf.js") {
		t.Errorf("expected a re-orient reply naming the file and fresh=true, got %q", v.Reply)
	}
	if !strings.Contains(v.Reply, "fresh=true") {
		t.Errorf("re-orient nudge should point at get fresh=true, got %q", v.Reply)
	}

	// Each nudge resets the streak, so the model gets maxFailureNudges guided rounds.
	for round := 1; round < maxFailureNudges; round++ {
		for i := 0; i < repeatedFailureNudgeThreshold; i++ {
			distinct(fmt.Sprintf("r%d_%d", round+1, i))
		}
		v = g.Before(st, nil, "manage_files", patchArgs("leaf.js", "again"))
		if v == nil || !v.Block || v.Stop {
			t.Fatalf("nudge round %d: expected a non-stopping nudge, got %v", round+1, v)
		}
	}
	// The nudges are spent: the target is refused for good, and stays refused,
	// but the session lives on so its other files can still be built.
	for i := 0; i < repeatedFailureNudgeThreshold; i++ {
		distinct(fmt.Sprintf("final_%d", i))
	}
	for i := 0; i < 2; i++ {
		v = g.Before(st, nil, "manage_files", patchArgs("leaf.js", "again"))
		if v == nil || !v.Block {
			t.Fatalf("refusal %d: expected a block once the nudges were spent, got %v", i, v)
		}
		if v.Stop {
			t.Fatalf("refusal %d: a per-target refusal must never force-stop the session", i)
		}
		if !strings.Contains(v.Reply, "repeated_failure_block") {
			t.Errorf("refusal %d: expected a repeated_failure_block reply, got %q", i, v.Reply)
		}
	}
}

// The distinct-failure backstop must be keyed per file, using the REAL path field
// (file_path), not the literal "path" key - a production manage_files call uses
// file_path, so flailing on one file must not force-stop an operation on another.
func TestRepeatGuard_DistinctFailureKeyedByRealPath(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	for i := 0; i < repeatedFailureNudgeThreshold+2; i++ {
		err := fmt.Sprintf(`{"success":false,"code":"syntax_error","error":"miss %d"}`, i)
		run(g, st, "manage_files", map[string]any{"action": "save", "file_path": "a.js", "body": "x"}, err, false)
	}
	// A first save on a DIFFERENT file (by file_path) must not inherit a.js's streak.
	if v := g.Before(st, nil, "manage_files", map[string]any{"action": "save", "file_path": "b.js"}); v != nil && v.Block {
		t.Fatalf("flailing on a.js must not block a first save on b.js (per-file keying via file_path), got %v", v)
	}
}

// The counter is keyed by path: flailing on one file must NOT block a first
// attempt on a DIFFERENT file.
func TestRepeatGuard_DoesNotBlockDifferentFile(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"syntax_error","error":"would introduce a syntax error in flight.js"}`
	for i := 0; i < repeatedResultBlockThreshold+1; i++ {
		run(g, st, "manage_files", patchArgs("flight.js", fmt.Sprintf("a%d", i)), sameErr, false)
	}
	if v := g.Before(st, nil, "manage_files", patchArgs("game.js", "x")); v != nil && v.Block {
		t.Fatalf("a patch on a different file must not inherit flight.js's streak, got %v", v)
	}
}

// A task is a target like any other: a task whose deliverable the model cannot get
// past the completion gate must not block completing every OTHER task in the plan.
// manage_tasks names its target with task_id, so without that field in the key
// every complete in the project shares one streak and one stuck task seals the
// checklist for the whole build.
func TestRepeatGuard_CompleteKeyedByTaskID(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	stuck := `{"success":false,"error":"task 'a' declares paths [booth] but no matching file, table, or endpoint exists yet"}`
	complete := func(id, result string) map[string]any {
		return map[string]any{"action": "complete", "task_id": id, "result": result}
	}
	for i := 0; i < repeatedResultBlockThreshold+2; i++ {
		run(g, st, "manage_tasks", complete("dj-logic-functions", fmt.Sprintf("try %d", i)), stuck, false)
	}
	if v := g.Before(st, nil, "manage_tasks", complete("dj-logic-functions", "again")); v == nil || !v.Block {
		t.Fatalf("the stuck task itself must still be blocked, got %v", v)
	}
	for _, id := range []string{"track-stream", "relay-positions", "sse-dj", "global-chat"} {
		if v := g.Before(st, nil, "manage_tasks", complete(id, "built and verified")); v != nil && v.Block {
			t.Fatalf("completing %s must not inherit the stuck task's streak, got %v", id, v)
		}
	}
}

// Endpoint tools name their target with endpoint_path, so flailing on one endpoint
// must not block a first attempt at another.
func TestRepeatGuard_KeyedByEndpointPath(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"validation_error","error":"same"}`
	for i := 0; i < repeatedResultBlockThreshold+1; i++ {
		args := map[string]any{"action": "create", "endpoint_path": "booth", "body": fmt.Sprintf("b%d", i)}
		run(g, st, "manage_function", args, sameErr, false)
	}
	if v := g.Before(st, nil, "manage_function", map[string]any{"action": "create", "endpoint_path": "tracks"}); v != nil && v.Block {
		t.Fatalf("a create on a different endpoint must not inherit booth's streak, got %v", v)
	}
}

// A call names its own target before its inputs: manage_api create declares BOTH
// endpoint_path and table_name, and the endpoint is the artifact it builds. Keying
// on the table instead would collapse every endpoint over that table into one
// streak - the same conflation, a different cause.
func TestRepeatGuard_EndpointPathWinsOverTableName(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"validation_error","error":"same"}`
	create := func(ep, extra string) map[string]any {
		return map[string]any{"action": "create", "endpoint_path": ep, "table_name": "posts", "methods": extra}
	}
	for i := 0; i < repeatedResultBlockThreshold+1; i++ {
		run(g, st, "manage_api", create("posts-feed", fmt.Sprintf("m%d", i)), sameErr, false)
	}
	// A second endpoint over the SAME table is a different target.
	if v := g.Before(st, nil, "manage_api", create("posts-admin", "GET")); v != nil && v.Block {
		t.Fatalf("a different endpoint over the same table must not inherit the streak, got %v", v)
	}
}

// A reject that advertises an override must let that override run. The deliverable
// gate points the model at force=true, and force is exactly "the other arguments
// varying" - the shape signal (1) refuses on purpose - so without an explicit hatch
// the platform blocks the escape it just recommended, and the target can never
// clear: a blocked call never runs After, so nothing can ever reset the streak.
func TestRepeatGuard_ForceRunsOncePerTarget(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	stuck := `{"success":false,"error":"declares paths [booth] but no matching endpoint exists yet"}`
	complete := func(force bool) map[string]any {
		a := map[string]any{"action": "complete", "task_id": "dj-logic-functions"}
		if force {
			a["force"] = true
			a["reason"] = "built as four function endpoints"
		}
		return a
	}
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		run(g, st, "manage_tasks", complete(false), stuck, false)
	}
	if v := g.Before(st, nil, "manage_tasks", complete(false)); v == nil || !v.Block {
		t.Fatalf("expected the unforced retry to be blocked, got %v", v)
	}
	if v := g.Before(st, nil, "manage_tasks", complete(true)); v != nil && v.Block {
		t.Fatalf("the advertised force retry must be allowed to run, got %v", v)
	}
	// One attempt only: a force that keeps failing is the flailing this guard stops.
	g.After(st, nil, "manage_tasks", complete(true), stuck, false)
	if v := g.Before(st, nil, "manage_tasks", complete(true)); v == nil || !v.Block {
		t.Fatalf("a second force on the same target must be refused, got %v", v)
	}
}

// The hatch is spent only against a standing refusal. force is legitimate on a
// first call (a task whose declared path genuinely cannot be produced), and that
// call was never blocked - so it must not cost the target the attempt it will need
// if a streak arms later.
func TestRepeatGuard_UnblockedForceDoesNotSpendHatch(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"error":"same"}`
	complete := func() map[string]any {
		return map[string]any{"action": "complete", "task_id": "audio-asset", "force": true, "reason": "binary"}
	}
	// Forced calls that were never refused: nothing to spend the hatch on.
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		if v := run(g, st, "manage_tasks", complete(), sameErr, false); v != nil && v.Block {
			t.Fatalf("blocked too early at attempt %d", i)
		}
	}
	// The streak has now armed, so the hatch is still available for one run past it.
	if v := g.Before(st, nil, "manage_tasks", complete()); v != nil && v.Block {
		t.Fatalf("the hatch must survive forces that were never refused, got %v", v)
	}
}

// Progress restores the hatch: once the target succeeds, a later streak on it gets
// its own force attempt rather than inheriting the spent one.
func TestRepeatGuard_ProgressRestoresForceHatch(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"error":"same"}`
	args := func(force bool) map[string]any {
		a := map[string]any{"action": "save", "file_path": "a.js"}
		if force {
			a["force"] = true
		}
		return a
	}
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		run(g, st, "manage_files", args(false), sameErr, false)
	}
	run(g, st, "manage_files", args(true), sameErr, false) // spends the hatch, still fails
	// The file lands via another action, which clears every streak on that target.
	run(g, st, "manage_files", map[string]any{"action": "patch", "file_path": "a.js"}, `{"success":true}`, true)
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		run(g, st, "manage_files", args(false), sameErr, false)
	}
	if v := g.Before(st, nil, "manage_files", args(true)); v != nil && v.Block {
		t.Fatalf("a success on the target must restore its force attempt, got %v", v)
	}
}

// Resolving a file via a DIFFERENT action (save) must clear that file's
// patch streak, so a later patch on it is not stale-blocked.
func TestRepeatGuard_SuccessOnPathViaOtherActionResets(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"syntax_error","error":"e"}`
	for i := 0; i < repeatedResultBlockThreshold; i++ {
		run(g, st, "manage_files", patchArgs("flight.js", fmt.Sprintf("a%d", i)), sameErr, false)
	}
	// Model fixes the file via save (a different action) - success clears the path's streaks.
	run(g, st, "manage_files", map[string]any{"action": "save", "path": "flight.js"}, `{"success":true}`, true)
	if v := g.Before(st, nil, "manage_files", patchArgs("flight.js", "x")); v != nil && v.Block {
		t.Fatalf("a successful write to the file (via any action) must clear its patch streak, got %v", v)
	}
}

// A successful call on the same file is progress and clears the streak, so failures
// resuming afterward start counting from zero.
func TestRepeatGuard_SuccessResetsResultStreak(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	sameErr := `{"success":false,"code":"unresolved_import","error":"same"}`

	// Two identical failures (below threshold), then a success, then two more - all on the same file.
	run(g, st, "manage_files", patchArgs("f.js", "a"), sameErr, false)
	run(g, st, "manage_files", patchArgs("f.js", "b"), sameErr, false)
	run(g, st, "manage_files", patchArgs("f.js", "c"), `{"success":true}`, true) // progress
	run(g, st, "manage_files", patchArgs("f.js", "d"), sameErr, false)
	run(g, st, "manage_files", patchArgs("f.js", "e"), sameErr, false)

	if v := g.Before(st, nil, "manage_files", patchArgs("f.js", "f")); v != nil && v.Block {
		t.Fatalf("success should have reset the streak; only 2 post-success failures, must not block: %v", v)
	}
}

// fixThenProbe drives one repair round: a write on the endpoint that SUCCEEDS,
// then the probe of it that fails. Returns the probe's verdict.
func fixThenProbe(g RepeatGuard, st *GuardState, endpoint, code, probeErr string) *Verdict {
	run(g, st, "manage_function", map[string]any{
		"action": "update", "endpoint_path": endpoint, "code": code,
	}, `{"success":true,"data":{"updated":true}}`, true)
	return run(g, st, "manage_testing", map[string]any{
		"action": "test_endpoint", "endpoint_path": endpoint, "method": "POST",
	}, probeErr, false)
}

// The gap this closes: every repair round lands a SUCCESSFUL write, which clears
// both the identical-result and the total-failure streaks for the probe, so
// signals (1) and (2) hold at one no matter how many rounds run. Only the cycle
// counter survives the clear, so it is what ends a fix-then-probe alternation
// whose fixes never change the reading.
func TestRepeatGuard_BlocksUnconvergingFixThenProbeCycle(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	probeErr := `{"success":false,"data":{"status":500},"error":"expected a 2xx status, got 500"}`

	for i := 0; i < repeatedProbeCycleThreshold; i++ {
		if v := fixThenProbe(g, st, "vote", fmt.Sprintf("v%d", i), probeErr); v != nil && v.Block {
			t.Fatalf("blocked too early at round %d", i)
		}
	}
	v := g.Before(st, nil, "manage_testing", map[string]any{
		"action": "test_endpoint", "endpoint_path": "vote", "method": "POST",
	})
	if v == nil || !v.Block {
		t.Fatalf("expected a block after %d unconverging rounds, got %v", repeatedProbeCycleThreshold, v)
	}
	if v.Stop {
		t.Error("one stuck probe must not force-stop the session; other targets stay reachable")
	}
	// The reply replays the unchanged failure, which is what the model has to re-read.
	if !strings.Contains(v.Reply, "got 500") {
		t.Errorf("reply must carry the unchanged failure, got %s", v.Reply)
	}
}

// A probe that PASSES is the evidence a fix landed, and the only thing that
// retires the cycle. Rounds resuming afterward start from zero.
func TestRepeatGuard_PassingProbeRetiresCycle(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	probeErr := `{"success":false,"error":"got 500"}`

	for i := 0; i < repeatedProbeCycleThreshold-1; i++ {
		fixThenProbe(g, st, "vote", fmt.Sprintf("v%d", i), probeErr)
	}
	run(g, st, "manage_testing", map[string]any{
		"action": "test_endpoint", "endpoint_path": "vote", "method": "POST",
	}, `{"success":true,"data":{"passed":true}}`, true)
	for i := 0; i < repeatedProbeCycleThreshold-1; i++ {
		if v := fixThenProbe(g, st, "vote", fmt.Sprintf("w%d", i), probeErr); v != nil && v.Block {
			t.Fatalf("a passing probe must reset the cycle, blocked at post-pass round %d", i)
		}
	}
}

// A CHANGED failure is new information - the fixes are moving the outcome - so
// the cycle resets on the same rule as the identical-result streak.
func TestRepeatGuard_ChangingProbeFailureResetsCycle(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	for i := 0; i < repeatedProbeCycleThreshold*2; i++ {
		err := fmt.Sprintf(`{"success":false,"error":"missing field %d"}`, i)
		if v := fixThenProbe(g, st, "vote", fmt.Sprintf("v%d", i), err); v != nil && v.Block {
			t.Fatalf("a probe reporting a DIFFERENT failure each round is converging, blocked at %d", i)
		}
	}
}

// The cycle is per (probe action + target), so a stuck endpoint never blocks the
// first probe of another one.
func TestRepeatGuard_ProbeCycleKeyedByTarget(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	probeErr := `{"success":false,"error":"got 500"}`
	for i := 0; i < repeatedProbeCycleThreshold+1; i++ {
		fixThenProbe(g, st, "vote", fmt.Sprintf("v%d", i), probeErr)
	}
	if v := g.Before(st, nil, "manage_testing", map[string]any{
		"action": "test_endpoint", "endpoint_path": "memberships", "method": "POST",
	}); v != nil && v.Block {
		t.Fatalf("a probe of a different endpoint must not inherit the stuck one's cycle, got %v", v)
	}
}

// The stall force-stop: the same call shape issued
// repeatedCallStallThreshold turns in a row - even succeeding - refuses and stops.
func TestRepeatGuard_StallForceStopPreserved(t *testing.T) {
	g, st := RepeatGuard{}, NewGuardState()
	args := map[string]any{"action": "list"} // identical, and it succeeds each time

	for i := 0; i < repeatedCallStallThreshold-1; i++ {
		if v := run(g, st, "manage_tasks", args, `{"success":true}`, true); v != nil && v.Block {
			t.Fatalf("blocked too early at attempt %d", i)
		}
	}
	v := g.Before(st, nil, "manage_tasks", args)
	if v == nil || !v.Block || !v.Stop {
		t.Fatalf("expected a force-stop after %d identical calls, got %v", repeatedCallStallThreshold, v)
	}
}
