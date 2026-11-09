package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"openfabrica/engine/enginetest"
	"openfabrica/pack"

	_ "modernc.org/sqlite"
)

// AmbitionTaskBudget scales with the level, ceiling is always >= target, off
// shares the lean low budget, unknown falls back to the high default, and the
// high ceiling sits at/under the largePlanTaskThreshold the run log already
// treats as oversized (so an oversized plan is reined in).
func TestAmbitionTaskBudget(t *testing.T) {
	for _, lvl := range []string{AmbitionLow, AmbitionMedium, AmbitionHigh} {
		target, ceiling := AmbitionTaskBudget(lvl)
		if target <= 0 || ceiling <= 0 {
			t.Fatalf("%s: budget must be positive, got target=%d ceiling=%d", lvl, target, ceiling)
		}
		if ceiling < target {
			t.Errorf("%s: ceiling (%d) must be >= target (%d)", lvl, ceiling, target)
		}
	}
	lt, lc := AmbitionTaskBudget(AmbitionLow)
	mt, mc := AmbitionTaskBudget(AmbitionMedium)
	ht, hc := AmbitionTaskBudget(AmbitionHigh)
	if !(lt < mt && mt < ht) || !(lc < mc && mc < hc) {
		t.Errorf("budget must grow with ambition: low(%d,%d) medium(%d,%d) high(%d,%d)", lt, lc, mt, mc, ht, hc)
	}
	if hc > largePlanTaskThreshold {
		t.Errorf("high ceiling (%d) must sit at/under the oversized-plan threshold (%d) to rein in an oversized plan", hc, largePlanTaskThreshold)
	}
	// off (skip EXPAND, take the brief literally) is the least-expansive end of the
	// dial, so it shares the lean LOW budget - keeping off <= low <= medium <= high.
	if ot, oc := AmbitionTaskBudget(AmbitionOff); ot != lt || oc != lc {
		t.Errorf("off must map to the lean low budget, got (%d,%d) want (%d,%d)", ot, oc, lt, lc)
	}
	// An unknown value is folded to the default (high) by NormalizeAmbition.
	if ut, uc := AmbitionTaskBudget("garbage"); ut != ht || uc != hc {
		t.Errorf("unknown level must map to the high default budget, got (%d,%d)", ut, uc)
	}
}

// AmbitionTaskFloor: only high has one, and it sits under the target. It no longer
// drives a retry; it only selects the firmer high-tier budget directive.
func TestAmbitionTaskFloor(t *testing.T) {
	ht, _ := AmbitionTaskBudget(AmbitionHigh)
	if f := AmbitionTaskFloor(AmbitionHigh); f <= 0 || f > ht {
		t.Errorf("high floor must be positive and <= target %d, got %d", ht, f)
	}
	for _, lvl := range []string{AmbitionOff, AmbitionLow, AmbitionMedium} {
		if f := AmbitionTaskFloor(lvl); f != 0 {
			t.Errorf("%s must have no floor, got %d", lvl, f)
		}
	}
}

// PlanTaskBudget is brief-aware only for the off case: low/medium/high ignore the
// brief and stay enforced (EXPAND normalised them), a terse off brief keeps the
// lean cap enforced, and a detailed off brief (owner-specified scope) becomes
// advisory so the plan is built as written.
func TestPlanTaskBudget_BriefAware(t *testing.T) {
	detailed := strings.Repeat("word ", ownerSpecifiedBriefWords+20)
	terse := "a huge open-world game"

	// Non-off levels: always enforced, brief ignored.
	for _, lvl := range []string{AmbitionLow, AmbitionMedium, AmbitionHigh} {
		if _, _, enf := PlanTaskBudget(lvl, detailed); !enf {
			t.Errorf("%s must stay enforced regardless of brief", lvl)
		}
	}

	// off + terse: the planner infers scope -> lean budget, enforced.
	lt, lc := AmbitionTaskBudget(AmbitionLow)
	if tg, cl, enf := PlanTaskBudget(AmbitionOff, terse); tg != lt || cl != lc || !enf {
		t.Errorf("off+terse must be the lean enforced budget, got (%d,%d,enforced=%v)", tg, cl, enf)
	}

	// off + detailed: owner-specified scope -> advisory (not enforced), generous size.
	ht, hc := AmbitionTaskBudget(AmbitionHigh)
	if tg, cl, enf := PlanTaskBudget(AmbitionOff, detailed); enf || tg != ht || cl != hc {
		t.Errorf("off+detailed must be advisory at the high size, got (%d,%d,enforced=%v)", tg, cl, enf)
	}
}

// The {{TASK_BUDGET}} sentinel substitutes into the rendered PLAN prompt: the
// directive carries the level's numbers and the core-first / exclusions rule, and
// no literal placeholder is left behind.
func TestPlanPromptSubstitutesTaskBudget(t *testing.T) {
	target, ceiling := AmbitionTaskBudget(AmbitionHigh)
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	sys := pack.Assemble(pack.AssembleInput{
		Stage:     StagePlan,
		Selection: pack.Select(ps, StagePlan),
		Sentinels: map[string]string{"TASK_BUDGET": planTaskBudgetDirective(AmbitionHigh, target, ceiling, true, false)},
	}).System
	if strings.Contains(sys, "{{TASK_BUDGET}}") {
		t.Fatalf("the TASK_BUDGET sentinel was not substituted")
	}
	for _, want := range []string{"Task budget", fmt.Sprintf("%d", target), fmt.Sprintf("%d", ceiling), "exclusions", "coherent core"} {
		if !strings.Contains(strings.ToLower(sys), strings.ToLower(want)) {
			t.Errorf("plan prompt missing %q", want)
		}
	}
}

// planJSONWithTasks builds a plan with n distinct, non-overlapping tasks (unique
// titles and owned paths, so nothing merges) - the count is exactly n.
func planJSONWithTasks(n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf(
			`{"title":"Feature %d","intent":"build feature %d","done_when":"feature %d works","paths":["/files/feat%d.js"]}`,
			i, i, i, i)
	}
	return `{"goal":"g","tasks":[` + strings.Join(parts, ",") + `]}`
}

// A plan over the ambition ceiling is pushed back ONCE and the trimmed re-emission
// is what seeds the queue: the exact reining-in an oversized build needs.
func TestPlanStage_EnforcesTaskBudget(t *testing.T) {
	_, ceiling := AmbitionTaskBudget(AmbitionMedium) // pinned to medium below: ceiling without floor interplay
	over := planJSONWithTasks(ceiling + 4)
	trimmed := planJSONWithTasks(ceiling - 2)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(over),    // first plan: over budget
		enginetest.Text(trimmed), // budget-retry: trimmed to core
	}}
	w, pdb := scheduledTestWorker(t, p)
	if _, err := w.deps.DB.ExecWrite("UPDATE projects SET ambition_level='medium' WHERE id=1"); err != nil {
		t.Fatalf("set medium: %v", err)
	}

	out, err := planStage{}.Run(context.Background(), w, &Run{ID: "run-budget", Kind: RunKindBuild})
	if err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if resolveNext(StagePlan, out) != StageBuild {
		t.Fatalf("plan should advance to build, got %+v", out)
	}
	if p.CallCount() != 4 {
		t.Fatalf("an over-budget plan must trigger exactly one corrective retry (guide-select + features + 2 plan calls), got %d", p.CallCount())
	}
	tasks, _ := LoadTasks(pdb.Reader())
	if len(tasks) != ceiling-2 {
		t.Fatalf("the trimmed %d-task plan must seed the queue, got %d", ceiling-2, len(tasks))
	}
}

// off + a DETAILED (owner-specified) brief is built as written: even a plan past
// the high ceiling is NOT retried or capped, because the owner did EXPAND's job
// themselves and their scope is honoured.
func TestPlanStage_OffDetailedBriefNotCapped(t *testing.T) {
	_, ceiling := AmbitionTaskBudget(AmbitionHigh)
	big := planJSONWithTasks(ceiling + 6) // well over any ceiling
	// A long brief that yields no features is asked once more, hence two feature replies.
	p := &enginetest.Provider{Steps: []enginetest.Step{selectNoGuides, noFeatures, noFeatures, enginetest.Text(big)}}
	w, pdb := scheduledTestWorker(t, p)

	detailed := strings.Repeat("feature spec ", ownerSpecifiedBriefWords) // >= owner-specified floor
	if _, err := w.deps.DB.ExecWrite(
		"UPDATE projects SET ambition_level='off', description=? WHERE id=1", detailed); err != nil {
		t.Fatalf("set off+detailed: %v", err)
	}

	out, err := planStage{}.Run(context.Background(), w, &Run{ID: "run-off-detailed", Kind: RunKindBuild})
	if err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if resolveNext(StagePlan, out) != StageBuild {
		t.Fatalf("plan should advance to build, got %+v", out)
	}
	// Two calls: the guide-selection pre-pass + one plan call. The point is NO
	// budget-retry (which would be a third call) - off+detailed is built as written.
	if p.CallCount() != 4 {
		t.Fatalf("off+detailed must NOT retry/cap the plan (guide-select + 2 feature calls + 1 plan = 4 calls), got %d", p.CallCount())
	}
	tasks, _ := LoadTasks(pdb.Reader())
	if len(tasks) != ceiling+6 {
		t.Fatalf("the full owner-specified plan must seed uncapped, got %d", len(tasks))
	}
}

// off + a TERSE brief keeps the lean cap enforced: a few words let the planner
// infer a large scope, so it is reined in like any oversized plan.
func TestPlanStage_OffTerseBriefCapped(t *testing.T) {
	_, lean := AmbitionTaskBudget(AmbitionLow)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSONWithTasks(lean + 4)), // over the lean ceiling
		enginetest.Text(planJSONWithTasks(lean - 1)), // trimmed
	}}
	w, pdb := scheduledTestWorker(t, p)

	if _, err := w.deps.DB.ExecWrite(
		"UPDATE projects SET ambition_level='off', description='a huge open-world game' WHERE id=1"); err != nil {
		t.Fatalf("set off+terse: %v", err)
	}

	if _, err := (planStage{}).Run(context.Background(), w, &Run{ID: "run-off-terse", Kind: RunKindBuild}); err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if p.CallCount() != 4 {
		t.Fatalf("off+terse must be capped (guide-select + features + one retry = 4 calls), got %d", p.CallCount())
	}
	tasks, _ := LoadTasks(pdb.Reader())
	if len(tasks) != lean-1 {
		t.Fatalf("the trimmed lean plan must seed, got %d", len(tasks))
	}
}

// A plan within budget seeds as-is with NO retry - the enforcement never adds a
// round-trip to a correctly-sized plan.
func TestPlanStage_UnderBudgetNoRetry(t *testing.T) {
	target, _ := AmbitionTaskBudget(AmbitionMedium)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSONWithTasks(target)),
	}}
	w, pdb := scheduledTestWorker(t, p)
	if _, err := w.deps.DB.ExecWrite("UPDATE projects SET ambition_level='medium' WHERE id=1"); err != nil {
		t.Fatalf("set medium: %v", err)
	}

	if _, err := (planStage{}).Run(context.Background(), w, &Run{ID: "run-ok", Kind: RunKindBuild}); err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if p.CallCount() != 3 {
		t.Fatalf("an in-budget plan must NOT retry (guide-select + features + 1 plan call), got %d calls", p.CallCount())
	}
	tasks, _ := LoadTasks(pdb.Reader())
	if len(tasks) != target {
		t.Fatalf("expected %d seeded tasks, got %d", target, len(tasks))
	}
}

// If the planner is STILL over budget after the one retry, the build proceeds with
// the retried plan rather than failing - shipping a large build beats shipping
// nothing (the over-scope is logged, not fatal).
func TestPlanStage_StillOverAfterRetryProceeds(t *testing.T) {
	_, ceiling := AmbitionTaskBudget(AmbitionMedium)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSONWithTasks(ceiling + 5)), // over
		enginetest.Text(planJSONWithTasks(ceiling + 2)), // retry: still over
	}}
	w, pdb := scheduledTestWorker(t, p)
	if _, err := w.deps.DB.ExecWrite("UPDATE projects SET ambition_level='medium' WHERE id=1"); err != nil {
		t.Fatalf("set medium: %v", err)
	}

	out, err := planStage{}.Run(context.Background(), w, &Run{ID: "run-still-over", Kind: RunKindBuild})
	if err != nil {
		t.Fatalf("a still-over plan must proceed, not error: %v", err)
	}
	if resolveNext(StagePlan, out) != StageBuild {
		t.Fatalf("plan should still advance to build, got %+v", out)
	}
	if p.CallCount() != 4 {
		t.Fatalf("exactly one retry (guide-select + features + 2 plan calls), got %d", p.CallCount())
	}
	tasks, _ := LoadTasks(pdb.Reader())
	if len(tasks) != ceiling+2 {
		t.Fatalf("the retried plan (%d tasks) must seed even though still over, got %d", ceiling+2, len(tasks))
	}
}

// An undershot high-ambition plan is NOT bounced for coverage: the floor retry was
// removed, so a thin first-pass plan seeds as written with no extra round-trip.
func TestPlanStage_UndershotHighDoesNotRetry(t *testing.T) {
	floor := AmbitionTaskFloor(AmbitionHigh) // scheduledTestWorker has no level -> high default
	p := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSONWithTasks(floor - 6)), // undershot, but accepted as-is
	}}
	w, pdb := scheduledTestWorker(t, p)

	out, err := planStage{}.Run(context.Background(), w, &Run{ID: "run-floor", Kind: RunKindBuild})
	if err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if resolveNext(StagePlan, out) != StageBuild {
		t.Fatalf("plan should advance to build, got %+v", out)
	}
	if p.CallCount() != 3 {
		t.Fatalf("an undershot high plan must NOT retry (guide-select + features + 1 plan call), got %d", p.CallCount())
	}
	tasks, _ := LoadTasks(pdb.Reader())
	if len(tasks) != floor-6 {
		t.Fatalf("the undershot %d-task plan must seed as written, got %d", floor-6, len(tasks))
	}
}

// A high plan at its target seeds with NO retry: the floor never adds a
// round-trip to a correctly-sized plan.
func TestPlanStage_HighAtTargetNoRetry(t *testing.T) {
	target, _ := AmbitionTaskBudget(AmbitionHigh)
	p := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSONWithTasks(target)),
	}}
	w, pdb := scheduledTestWorker(t, p)

	if _, err := (planStage{}).Run(context.Background(), w, &Run{ID: "run-high-ok", Kind: RunKindBuild}); err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if p.CallCount() != 3 {
		t.Fatalf("a target-sized high plan must NOT retry (guide-select + features + 1 plan call), got %d", p.CallCount())
	}
	tasks, _ := LoadTasks(pdb.Reader())
	if len(tasks) != target {
		t.Fatalf("expected %d seeded tasks, got %d", target, len(tasks))
	}
}
