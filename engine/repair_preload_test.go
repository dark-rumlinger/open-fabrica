package engine

import (
	"slices"
	"testing"
)

func TestRepairPreloadTools_IncludesRepairIntentTools(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	_, err := AddTasks(pdb, "run_1", []Task{{
		ID:     "fix-1-webfont-css-app-css",
		Title:  "Fix webfont on css/app.css",
		Intent: "Declare the family (`manage_libraries action=add_library`) so both families load.",
		Origin: TaskOriginRepair,
	}})
	if err != nil {
		t.Fatalf("add repair task: %v", err)
	}

	got := repairPreloadTools(pdb.Reader(), &Plan{}, w.deps.ToolRegistry)
	if !slices.Contains(got, "manage_libraries") {
		t.Fatalf("repairPreloadTools must preload manage_libraries when named in repair intent, got %v", got)
	}
}
