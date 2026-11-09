package engine

import "testing"

// TestMergeOverlappingPlanTasks: two tasks that OWN the same path are folded
// into one (deps re-pointed), an edits-only overlap is left alone, and a merge
// that would create a dependency cycle is skipped.
func TestMergeOverlappingPlanTasks(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)

	// Same-page features split into three tasks all owning list.js, plus a
	// downstream task depending on one of them.
	in := []PlanTaskSpec{
		{ID: "list", Title: "Daily list", Intent: "render list", Paths: []string{"/files/list.js"}},
		{ID: "form", Title: "Create form", Intent: "add form", Paths: []string{"files/list.js"}},
		{ID: "grid", Title: "Weekly grid", Intent: "grid", Paths: []string{"/files/grid.js"}, Deps: []string{"form"}},
	}
	out := mergeOverlappingPlanTasks(w, in)

	if len(out) != 2 {
		t.Fatalf("expected list+form merged into 2 tasks, got %d: %+v", len(out), out)
	}
	if out[0].ID != "list" {
		t.Fatalf("survivor should be the earlier task 'list', got %q", out[0].ID)
	}
	if !containsStr(out[0].Paths, "/files/list.js") {
		t.Fatalf("merged task lost its owned path: %+v", out[0].Paths)
	}
	// The grid's dep on the merged-away 'form' is re-pointed to the survivor.
	grid := out[1]
	if grid.ID != "grid" || len(grid.Deps) != 1 || grid.Deps[0] != "list" {
		t.Fatalf("grid dep should re-point form->list, got %+v", grid.Deps)
	}

	// Edits-only overlap is the intended cross-module pattern, not a conflict.
	noMerge := mergeOverlappingPlanTasks(w, []PlanTaskSpec{
		{ID: "list", Title: "List", Paths: []string{"/files/list.js"}},
		{ID: "form", Title: "Form", Paths: []string{"/files/form.js"}, Edits: []string{"/files/list.js"}, Deps: []string{"list"}},
	})
	if len(noMerge) != 2 {
		t.Fatalf("edits overlap must not merge, got %d tasks", len(noMerge))
	}

	// A merge that would cycle (A deps C, C deps B, A&B share a path) is skipped.
	cyc := mergeOverlappingPlanTasks(w, []PlanTaskSpec{
		{ID: "a", Title: "A", Paths: []string{"/files/x.js"}, Deps: []string{"c"}},
		{ID: "b", Title: "B", Paths: []string{"/files/x.js"}},
		{ID: "c", Title: "C", Deps: []string{"b"}},
	})
	if len(cyc) != 3 {
		t.Fatalf("cycle-creating merge must be skipped, got %d tasks", len(cyc))
	}
}

// A page route and a same-stem backend resource are DIFFERENT namespaces and
// must never fold: "/activity" (a page) merged into "activity" (a table/API)
// buries the page inside the backend task, which the builder completes once the
// API exists - and the planned page is silently dropped. Two tasks that genuinely
// own the same page route still merge.
func TestMergeOverlappingPlanTasks_PageRouteVsResource(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)

	out := mergeOverlappingPlanTasks(w, []PlanTaskSpec{
		{ID: "activity-table", Title: "Activity log table + API", Paths: []string{"activity"}},
		{ID: "activity-page", Title: "Activity page", Paths: []string{"/activity"}, Deps: []string{"activity-table"}},
	})
	if len(out) != 2 {
		t.Fatalf("a page route and a same-stem table must stay two tasks, got %d: %+v", len(out), out)
	}

	// Two real page tasks owning the same route are a genuine conflict: still merge.
	dup := mergeOverlappingPlanTasks(w, []PlanTaskSpec{
		{ID: "feed-a", Title: "Feed A", Paths: []string{"/activity"}},
		{ID: "feed-b", Title: "Feed B", Paths: []string{"/activity"}},
	})
	if len(dup) != 1 {
		t.Fatalf("two tasks owning the same page route must merge, got %d: %+v", len(dup), dup)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
