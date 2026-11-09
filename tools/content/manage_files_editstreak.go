package content

// The edit-streak / anti-loop state machine for manage_files: it tracks repeated
// no-op patches, anchor misses, and resave churn per file and escalates with a
// stop directive so the model breaks out of a stuck edit loop. The write
// handlers (save / patch / bulk_replace) in manage_files_write.go call into it.

import (
	"fmt"

	"openfabrica/internal/editbudget"
	"openfabrica/internal/replaybudget"
	"openfabrica/tools"
)

// noopStreakKey namespaces the no-change patch streak apart from the anchor-miss
// streak on the same file, so each escalation fires only for its own failure shape.
func noopStreakKey(kind, path string) string {
	return tools.ReadCacheKey(kind, path) + "\x00noop"
}

// noopCumulativeStop is the per-file no-change patches past which the write path
// gives the decisive stop directive, even when they never stacked up back to back.
// noopRunTotalStop is the same across every file in the run, so a model spreading
// no-ops over many files (each reset by an unrelated success) is still caught. The
// consecutive streak (>=2) catches a tight loop; these catch the distributed shape,
// which is how the no-op loop actually runs in practice.
const (
	noopCumulativeStop = 4
	noopRunTotalStop   = 4
)

// editFailDurable reports whether this call can use the per-run durable failure
// store: the engine tool loop sets RunID and DB for every non-LIVE stage. Off a run
// (LIVE chat, tests) the in-memory ReadTracker is the fallback.
func editFailDurable(ctx *tools.ToolContext) bool {
	return ctx != nil && ctx.RunID != "" && ctx.DB != nil
}

// recordAnchorMiss records one patch that could not be applied to the file and
// returns the failures in a row on it. Durable per run so the re-save escalation
// survives the segment rollover and build->repair restart that reset the in-memory
// counter; the tracker is the fallback off a run.
func recordAnchorMiss(ctx *tools.ToolContext, kind, path string) int {
	if editFailDurable(ctx) {
		return editbudget.Record(ctx.DB, ctx.RunID, editbudget.Anchor, tools.ReadCacheKey(kind, path))
	}
	return ctx.ReadTracker.RecordEditFailure(tools.ReadCacheKey(kind, path))
}

// resaveStopSuffix is the decisive directive appended once a file's patch failures
// reach the cap: the model is patching a copy that keeps drifting, so stop and
// re-save the whole body, which carries its own complete text and cannot drift.
func resaveStopSuffix(path string, fails int) string {
	return fmt.Sprintf(" STOP: that is %d patches in a row that could not be applied to %s. If the fix above does not land on the next try, stop patching this file and re-`save` the whole corrected body instead (a full save carries your own complete text and cannot drift).", fails, path)
}

// escalateStuckPatch records a patch-location failure that carried no circuit-breaker
// of its own (anchor consumed, chained hunk, refused fuzzy deletion) and, once the
// file's failures in a row reach the cap, appends the decisive re-save directive so
// the model stops looping on identical feedback (triage item 5).
func escalateStuckPatch(ctx *tools.ToolContext, kind, path string, res *tools.Result) *tools.Result {
	if res == nil || res.Success {
		return res
	}
	if fails := recordAnchorMiss(ctx, kind, path); fails >= 2 {
		if data, ok := res.Data.(map[string]any); ok {
			data["edit_loop"] = fails
		}
		res.Error += resaveStopSuffix(path, fails)
	}
	return res
}

// noopStopDirective records one no-change patch reject and returns the decisive stop
// message once any threshold is met, plus the consecutive and per-file cumulative
// counts for the result data. Consecutive is a per-segment signal (a tight loop in
// one session); the cumulative per-file count and the run total are durable per run
// so the STOP survives the build->repair restart the in-memory counter reset on. It
// must be called exactly once per no-op reject (it increments the counts); the
// directive is empty below every threshold. Shared by both no-op reject shapes.
func noopStopDirective(ctx *tools.ToolContext, kind, path string) (directive string, consecutive, cumulative int) {
	consecutive = ctx.ReadTracker.RecordEditFailure(noopStreakKey(kind, path))
	runTotal := 0
	if editFailDurable(ctx) {
		cumulative = editbudget.Record(ctx.DB, ctx.RunID, editbudget.NoOp, tools.ReadCacheKey(kind, path))
		runTotal = editbudget.RunTotal(ctx.DB, ctx.RunID, editbudget.NoOp)
	} else {
		cumulative = ctx.ReadTracker.CumulativeEditFailures(noopStreakKey(kind, path))
		runTotal = cumulative
	}
	if consecutive < 2 && cumulative < noopCumulativeStop && runTotal < noopRunTotalStop {
		return "", consecutive, cumulative
	}
	count, where := consecutive, "in a row on "+path
	switch {
	case consecutive >= 2:
	case cumulative >= noopCumulativeStop:
		count, where = cumulative, "on "+path+" across this build"
	default:
		count, where = runTotal, "across this build (on several files)"
	}
	return fmt.Sprintf(
		" STOP: that is %d no-change patches %s. Re-sending a hunk whose `replace` already equals `search` can NEVER change the file. If it already holds what you intended there, the edit is DONE - move on; to ADD text use insert_after / insert_before; send search/replace only when `replace` genuinely differs.",
		count, where), consecutive, cumulative
}

// addBlindEditNote appends a one-shot reminder to a successful app write once the
// run's user-flow replay budget is spent: no replay is left to verify further edits,
// so the model is steered to make only the fix it is sure of and lean on the post-build
// sweep. Keyed off the durable replay budget (Left==0 means all replays taken this
// run), it fires only during or after the flow phase - before it no flow replays have
// run, so the budget is never exhausted - and at most once per run. Inert off a run
// (LIVE chat, tests). This is the soft side of the accepted BlindEdits gap (triage #3):
// it advises, never blocks, so a legitimate final fix is not refused.
func addBlindEditNote(ctx *tools.ToolContext, data map[string]any) {
	if !editFailDurable(ctx) {
		return
	}
	scope := replaybudget.Scope{DB: ctx.DB, ProjectID: ctx.ProjectID, RunID: ctx.RunID}
	if replaybudget.Left(scope) > 0 {
		return
	}
	if editbudget.Record(ctx.DB, ctx.RunID, editbudget.Nudge, "blind") != 1 {
		return // already nudged this run
	}
	data["verify_note"] = "the user-flow replay budget for this run is spent, so this edit cannot be verified by a replay - make only the fix you are sure of and stop; the post-build check replays the flows once more and reports anything still red."
}

// clearEditStreaks resets the per-file failure streaks after a successful write. The
// in-memory tracker is always cleared; the durable anchor-miss streak is cleared too
// when on a run. The durable no-op run total is never cleared: it counts wasted
// no-change round-trips for the whole run.
func clearEditStreaks(ctx *tools.ToolContext, kind, path string) {
	ctx.ReadTracker.ClearEditFailures(tools.ReadCacheKey(kind, path))
	ctx.ReadTracker.ClearEditFailures(noopStreakKey(kind, path))
	if editFailDurable(ctx) {
		editbudget.Clear(ctx.DB, ctx.RunID, editbudget.Anchor, tools.ReadCacheKey(kind, path))
	}
}
