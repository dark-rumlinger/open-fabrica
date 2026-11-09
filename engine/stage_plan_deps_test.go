package engine

import "testing"

// taskByID loads the seeded checklist and returns the row with the given id.
func taskByID(t *testing.T, w *Worker, id string) Task {
	t.Helper()
	tasks, err := LoadTasks(w.pdb.Reader())
	if err != nil {
		t.Fatalf("load tasks: %v", err)
	}
	for _, tk := range tasks {
		if tk.ID == id {
			return tk
		}
	}
	t.Fatalf("no seeded task with id %q (have %d)", id, len(tasks))
	return Task{}
}

// The planner may omit `id` on every task (so ids derive from titles) yet declare
// deps by a short natural slug. Resolution must run against the ASSIGNED ids:
// indexing only explicit ids leaves `known` empty and drops every dep with a
// warning, silently discarding the page-before-table ordering. Matching covers
// both a full title-slug (`shared-layout-and-design-system`) and a unique token
// prefix (`projects-table` -> `projects-table-crud-api`).
func TestSeedTasksFromPlan_ResolvesDepsToAssignedIDs(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	r := &Run{ID: "run-test"}

	plan := &Plan{Tasks: []PlanTaskSpec{
		{Title: "Projects table + CRUD API", Paths: []string{"projects"}},
		{Title: "Shared layout and design system"},
		{Title: "Home page", Deps: []string{"projects-table", "shared-layout-and-design-system"}},
		{Title: "Contact page with form", Deps: []string{"contact-messages-table", "shared-layout-and-design-system"}},
		{Title: "Contact messages table + POST endpoint", Paths: []string{"messages"}},
	}}

	if n := seedTasksFromPlan(w, r, plan); n != 5 {
		t.Fatalf("expected 5 seeded tasks, got %d", n)
	}

	home := taskByID(t, w, "home-page")
	if len(home.Deps) != 2 {
		t.Fatalf("home page should keep both deps, got %+v", home.Deps)
	}
	if !containsStr(home.Deps, "projects-table-crud-api") {
		t.Fatalf("short-prefix dep 'projects-table' must resolve to 'projects-table-crud-api', got %+v", home.Deps)
	}
	if !containsStr(home.Deps, "shared-layout-and-design-system") {
		t.Fatalf("full-title-slug dep must resolve, got %+v", home.Deps)
	}

	contact := taskByID(t, w, "contact-page-with-form")
	if !containsStr(contact.Deps, "contact-messages-table-post-endpoint") {
		t.Fatalf("'contact-messages-table' must prefix-resolve to the POST-endpoint task, got %+v", contact.Deps)
	}
}

// An explicit id is honoured verbatim and a dep referencing it resolves exactly,
// while a genuinely dangling dep is still dropped (not invented).
func TestSeedTasksFromPlan_ExplicitIDsAndDanglingDrop(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	r := &Run{ID: "run-test"}

	plan := &Plan{Tasks: []PlanTaskSpec{
		{ID: "projects-table", Title: "Projects table"},
		{Title: "Home page", Deps: []string{"projects-table", "no-such-task"}},
	}}
	seedTasksFromPlan(w, r, plan)

	home := taskByID(t, w, "home-page")
	if len(home.Deps) != 1 || home.Deps[0] != "projects-table" {
		t.Fatalf("explicit-id dep should resolve and the dangling one drop, got %+v", home.Deps)
	}
}

// A task that lists itself as a dependency (a possible planner slip)
// must not seed a self-edge - that task could never become ready.
func TestSeedTasksFromPlan_SelfDepDropped(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	r := &Run{ID: "run-test"}

	plan := &Plan{Tasks: []PlanTaskSpec{
		{ID: "projects-table", Title: "Projects table", Deps: []string{"projects-table"}},
	}}
	seedTasksFromPlan(w, r, plan)

	tbl := taskByID(t, w, "projects-table")
	if len(tbl.Deps) != 0 {
		t.Fatalf("a self-dependency must be dropped, got %+v", tbl.Deps)
	}
}

// An ambiguous short prefix (matching two assigned ids) is left unresolved
// rather than guessing, so it drops instead of pointing at the wrong task.
func TestSeedTasksFromPlan_AmbiguousPrefixDropped(t *testing.T) {
	w, _ := scheduledTestWorker(t, nil)
	r := &Run{ID: "run-test"}

	plan := &Plan{Tasks: []PlanTaskSpec{
		{Title: "Orders table sync"},   // -> orders-table-sync
		{Title: "Orders table export"}, // -> orders-table-export
		{Title: "Dashboard", Deps: []string{"orders-table"}},
	}}
	seedTasksFromPlan(w, r, plan)

	dash := taskByID(t, w, "dashboard")
	if len(dash.Deps) != 0 {
		t.Fatalf("an ambiguous prefix dep must drop, not guess, got %+v", dash.Deps)
	}
}
