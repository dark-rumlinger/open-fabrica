package engine

import (
	"fmt"
	"strings"
	"testing"
)

// Only deterministic errors become repair items; warnings and speculative guesses do
// not, a red flow does, and findings of one category on one page are one item.
func TestRepairFindings(t *testing.T) {
	issues := []Issue{
		{Severity: "error", Category: "inert_click", Page: "/staff", Deterministic: true, Message: "Undo does nothing"},
		{Severity: "error", Category: "flow", Deterministic: true, Message: "flow red"},
		{Severity: "warning", Category: "inert_click", Page: "/", Deterministic: true, Message: "icon does nothing"},
		{Severity: "error", Category: "runtime", Page: "/", Deterministic: false, Message: "flaky"},
	}
	got := repairFindings(issues)
	if len(got) != 2 {
		t.Fatalf("repairFindings = %+v", got)
	}
	tasks := repairTasks(got, 1)
	if len(tasks) != 2 {
		t.Fatalf("repairTasks = %+v", tasks)
	}
	for _, tk := range tasks {
		if tk.Origin != TaskOriginRepair || !strings.HasPrefix(tk.ID, "fix-1-") {
			t.Fatalf("repair item = %+v", tk)
		}
	}
	if tasks[1].Title != "Fix a control that does nothing on /staff" && tasks[0].Title != "Fix a control that does nothing on /staff" {
		t.Fatalf("titles = %q, %q", tasks[0].Title, tasks[1].Title)
	}

	var many []Issue
	for i := 0; i < maxRepairItems+5; i++ {
		many = append(many, Issue{Severity: "error", Category: "dead_link", Page: fmt.Sprintf("/p%d", i), Deterministic: true, Message: "x"})
	}
	if n := len(repairTasks(repairFindings(many), 1)); n != maxRepairItems {
		t.Fatalf("items capped at %d, got %d", maxRepairItems, n)
	}
}

// Seven controls covered by one overlay are one repair item carrying every message.
func TestRepairTasksGroupByPageAndCategory(t *testing.T) {
	var covered []Issue
	for i := 0; i < 7; i++ {
		covered = append(covered, Issue{Severity: "error", Category: "covered_control", Page: "/", Deterministic: true,
			Signature: fmt.Sprintf("covered|desktop|b%d", i), Message: fmt.Sprintf("button b%d is covered", i)})
	}
	tasks := repairTasks(covered, 1)
	if len(tasks) != 1 || !strings.Contains(tasks[0].Intent, "b0") || !strings.Contains(tasks[0].Intent, "b6") {
		t.Fatalf("want one grouped item, got %+v", tasks)
	}
}

// A repair item completed as "disputed:" drops its explore findings from later sweeps,
// but only for the categories that are heuristics.
func TestRepairDisputes(t *testing.T) {
	w := &Worker{}
	inert := Issue{Severity: "error", Category: "inert_click", Page: "/", Deterministic: true, Signature: "inert|1"}
	clickErr := Issue{Severity: "error", Category: "click_error", Page: "/", Deterministic: true, Signature: "err|1"}
	w.repairSignatures([]Issue{inert, clickErr}, 1)
	if got := w.repairSigs[repairTaskID(inert, 1)]; len(got) != 1 {
		t.Fatalf("repairSigs = %+v", w.repairSigs)
	}
	w.disputed = map[string]bool{"inert_click\x00inert|1": true}
	got := w.dropDisputed([]Issue{inert, clickErr})
	if len(got) != 1 || got[0].Category != "click_error" {
		t.Fatalf("dropDisputed = %+v", got)
	}
}

// A round that does not reduce the findings is not progress.
func TestRepairProgress(t *testing.T) {
	a := Issue{Severity: "error", Category: "runtime", Page: "/a", Deterministic: true}
	b := Issue{Severity: "error", Category: "runtime", Page: "/b", Deterministic: true}
	if !repairProgress([]Issue{a, b}, []Issue{a}) {
		t.Error("fewer findings is progress")
	}
	if repairProgress([]Issue{a}, []Issue{a}) {
		t.Error("the same findings is not progress")
	}
	// A fix that broke a flow: one gap left, one regression added. The count went
	// 1 -> 2, yet the next round must run to repair what this one broke.
	gap := Issue{Severity: "error", Category: specGapCategory, Deterministic: true, Signature: specGapCategory + "|landing-join"}
	flow := Issue{Severity: "error", Category: "flow", Page: "/", Deterministic: true, Signature: "flow|bad join stays on the form|7|assert"}
	if !repairProgress([]Issue{gap}, []Issue{gap, flow}) {
		t.Error("a regression introduced by the round earns another round")
	}
	// The same flow failing on a freshly minted page is the same finding.
	moved := flow
	moved.Page = "/f/97f5c6a1"
	if repairProgress([]Issue{flow}, []Issue{moved}) {
		t.Error("a signed finding that only changed page is not progress")
	}
}

// What the repair rounds left of style drift ships as a warning, other findings unchanged.
func TestStyleDriftAsWarning(t *testing.T) {
	in := []Issue{
		{Severity: "error", Category: "style_drift", Page: "/a", Deterministic: true},
		{Severity: "error", Category: "runtime", Page: "/b", Deterministic: true},
	}
	out := styleDriftAsWarning(in)
	if out[0].Severity != "warning" || out[0].Deterministic || out[1].Severity != "error" || !out[1].Deterministic {
		t.Fatalf("styleDriftAsWarning = %+v", out)
	}
	if in[0].Severity != "error" {
		t.Fatal("the input must not be modified")
	}
	if len(FixableErrors(out)) != 1 {
		t.Fatalf("a lowered style drift is not fixable: %+v", FixableErrors(out))
	}
}
