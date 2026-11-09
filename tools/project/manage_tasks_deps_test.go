package project

import (
	"strings"
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// The defect this guard closes: a subsystem leaf completed before the spine/entry
// task that owns its shared contract. The planner expresses "entry first, then
// leaves" by making each leaf task deps on the spine - but until this guard that
// order was advisory, so a model could close a leaf before the entry existed and
// invent a window.__ bridge to a contract that wasn't there yet. complete must
// refuse a task while a declared dep is unfinished, naming the blocker.
func TestComplete_BlockedByIncompleteDep(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('spine', 'App spine + entry', 'active', 1)`); err != nil {
		t.Fatalf("seed spine: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, deps, paths, position)
		VALUES ('surface', 'Surface subsystem', 'active', '["spine"]', '["/files/app.surface.js"]', 2)`); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	seedFile(t, db, "file", "/files/app.surface.js")

	res, err := (&TasksComplete{TaskID: "surface", Result: "built surface"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("completing a leaf before its spine dep is done must be refused, got %+v", res)
	}
	if !strings.Contains(res.Error, "spine") {
		t.Errorf("refusal should name the incomplete dep, got: %s", res.Error)
	}
	if s := taskStatus(t, db, "surface"); s != "active" {
		t.Errorf("blocked task must stay un-done, got status %q", s)
	}
}

// Once the spine dep is done, the leaf completes normally.
func TestComplete_AllowedWhenDepDone(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('spine', 'App spine + entry', 'done', 1)`); err != nil {
		t.Fatalf("seed spine: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, deps, paths, position)
		VALUES ('surface', 'Surface subsystem', 'active', '["spine"]', '["/files/app.surface.js"]', 2)`); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	seedFile(t, db, "file", "/files/app.surface.js")

	res, err := (&TasksComplete{TaskID: "surface", Result: "built surface"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("leaf must complete once its spine dep is done, got %+v", res)
	}
	if s := taskStatus(t, db, "surface"); s != "done" {
		t.Errorf("status = %q, want done", s)
	}
}

// A dependency whose deliverables are already BUILT is not a blocker, even if the dep
// task itself is not yet marked done. This is the batch-completion case: the model saved
// every module, then completes all tasks in one turn - the gate must not reject a
// dependent just because the model marked it before its (already-built) dependency in the
// same batch. The dep's shared contract exists on disk; completion order is bookkeeping.
func TestComplete_AllowedWhenDepBuiltNotDone(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}
	// The dependency owns a file and has BUILT it, but is still 'active' (not marked done).
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('api', 'Discoveries API', 'active', '["/files/discoveries.api.js"]', 1)`); err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, deps, paths, position)
		VALUES ('leaf', 'Discoveries leaf', 'active', '["api"]', '["/files/discoveries.leaf.js"]', 2)`); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	seedFile(t, db, "file", "/files/discoveries.api.js")
	seedFile(t, db, "file", "/files/discoveries.leaf.js")

	res, err := (&TasksComplete{TaskID: "leaf", Result: "built leaf"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a dependent must complete when its dependency's deliverables are built, got %+v", res)
	}
	if s := taskStatus(t, db, "leaf"); s != "done" {
		t.Errorf("status = %q, want done", s)
	}
}

// The protection is preserved: a dependency that DECLARES deliverables but has built
// NOTHING still blocks - that is the entry-not-built case the guard exists for.
func TestComplete_BlockedWhenDepDeclaredPathsUnbuilt(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, paths, position)
		VALUES ('api', 'Discoveries API', 'active', '["/files/discoveries.api.js"]', 1)`); err != nil {
		t.Fatalf("seed dep: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, deps, paths, position)
		VALUES ('leaf', 'Discoveries leaf', 'active', '["api"]', '["/files/discoveries.leaf.js"]', 2)`); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	// Only the leaf's own file is built; the dependency 'api' built nothing.
	seedFile(t, db, "file", "/files/discoveries.leaf.js")

	res, err := (&TasksComplete{TaskID: "leaf", Result: "built leaf"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("a dependent must be refused while its dependency has built nothing, got %+v", res)
	}
	if !strings.Contains(res.Error, "api") {
		t.Errorf("refusal should name the unbuilt dep, got: %s", res.Error)
	}
	if s := taskStatus(t, db, "leaf"); s != "active" {
		t.Errorf("blocked task must stay un-done, got status %q", s)
	}
}

// A dep id that resolves to no task (a plan trimmed at the ambition ceiling, or a
// merge that dropped the id) must NOT block - a dangling dep can never wedge a task
// uncompletable.
func TestComplete_DanglingDepIgnored(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, deps, paths, position)
		VALUES ('leaf', 'Leaf', 'active', '["ghost"]', '["/files/leaf.js"]', 1)`); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	seedFile(t, db, "file", "/files/leaf.js")

	res, err := (&TasksComplete{TaskID: "leaf", Result: "built leaf"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("a dangling dep must be ignored, not block completion, got %+v", res)
	}
}

// force=true (with a reason) bypasses the dep guard exactly as it bypasses the
// deliverable guard - the auditable escape for a genuinely out-of-order completion.
func TestComplete_ForceBypassesDepGuard(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('spine', 'App spine', 'active', 1)`); err != nil {
		t.Fatalf("seed spine: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, deps, paths, position)
		VALUES ('surface', 'Surface', 'active', '["spine"]', '["/files/app.surface.js"]', 2)`); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}

	res, err := (&TasksComplete{TaskID: "surface", Force: true, Reason: "spine merged into this task"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("force must bypass the dep guard, got %+v", res)
	}
	if s := taskStatus(t, db, "surface"); s != "done" {
		t.Errorf("status = %q, want done", s)
	}
}

// Several incomplete deps are all named, with subject-verb agreement ("are").
func TestComplete_MultipleIncompleteDepsListed(t *testing.T) {
	db := setupTasksDB(t)
	ctx := &tools.ToolContext{DB: db, ProjectID: 1}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('table', 'Table', 'active', 1)`); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, position) VALUES ('spine', 'Spine', 'active', 2)`); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_tasks (id, title, status, deps, paths, position)
		VALUES ('leaf', 'Leaf', 'active', '["table","spine"]', '["/files/leaf.js"]', 3)`); err != nil {
		t.Fatalf("seed leaf: %v", err)
	}
	seedFile(t, db, "file", "/files/leaf.js")

	res, err := (&TasksComplete{TaskID: "leaf"}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("must be refused while two deps are incomplete, got %+v", res)
	}
	if !strings.Contains(res.Error, "table") || !strings.Contains(res.Error, "spine") {
		t.Errorf("refusal should name both blockers, got: %s", res.Error)
	}
	if !strings.Contains(res.Error, "are") {
		t.Errorf("refusal should agree in number (\"are\"), got: %s", res.Error)
	}
}
