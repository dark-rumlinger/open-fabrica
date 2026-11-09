package replaybudget

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// flowBudgetDDL matches the of_flow_budget table in db/project_migrations/001_initial.sql.
const flowBudgetDDL = `CREATE TABLE IF NOT EXISTS of_flow_budget (
    run_id        TEXT PRIMARY KEY,
    replays       INTEGER NOT NULL DEFAULT 0,
    best          INTEGER NOT NULL DEFAULT -1,
    best_depth    INTEGER NOT NULL DEFAULT 0,
    partial_depth INTEGER NOT NULL DEFAULT 0,
    stall_count   INTEGER NOT NULL DEFAULT 0,
    red           INTEGER NOT NULL DEFAULT 0,
    updated_at    DATETIME DEFAULT CURRENT_TIMESTAMP
)`

func openBudgetDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one connection keeps the write transaction and reads consistent
	if _, err := db.Exec(flowBudgetDDL); err != nil {
		t.Fatal(err)
	}
	return db
}

// The durable budget bounds a whole run and survives a process restart: the count
// lives in the DB, so reopening the database (a fresh process with empty in-memory
// state) keeps it spent, where the old in-memory counter reset and handed out a
// fresh allowance at the build->repair boundary.
func TestDurableBudgetSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	db := openBudgetDB(t, path)
	run := Scope{DB: db, ProjectID: 1, RunID: "run_a"}
	for i := 0; i < Max; i++ {
		if !Take(run) {
			t.Fatalf("replay %d must be within budget", i+1)
		}
	}
	if Take(run) {
		t.Fatal("the replay past Max must be refused")
	}
	if got := Left(run); got != 0 {
		t.Fatalf("left = %d, want 0", got)
	}
	db.Close()

	// Simulated restart: a fresh handle to the same file. An in-memory budget would
	// be empty here; the durable one is still spent.
	db2 := openBudgetDB(t, path)
	defer db2.Close()
	if Take(Scope{DB: db2, ProjectID: 1, RunID: "run_a"}) {
		t.Fatal("budget must still be spent after a restart")
	}
	// A different run gets its own fresh budget.
	other := Scope{DB: db2, ProjectID: 1, RunID: "run_b"}
	if !Take(other) {
		t.Fatal("a different run has its own budget")
	}
	if got := Left(other); got != Max-1 {
		t.Fatalf("other run left = %d, want %d", got, Max-1)
	}
}

// Durable stalls mirror the in-memory tracker: reds that get no closer to green
// reach the cap, and a green full run clears the stall state but never refunds the
// replays already taken.
func TestDurableStallsAndGreenKeepsReplays(t *testing.T) {
	db := openBudgetDB(t, filepath.Join(t.TempDir(), "p.db"))
	defer db.Close()
	run := Scope{DB: db, ProjectID: 1, RunID: "run_c"}
	Take(run)
	Take(run) // two replays spent, to prove a green does not give them back
	if got := Observe(run, 3, 0, true); got != 0 {
		t.Fatalf("the first red sets the baseline, got %d", got)
	}
	for want := 1; want <= MaxStalls; want++ {
		if got := Observe(run, 3, 0, true); got != want {
			t.Fatalf("stall %d: got %d", want, got)
		}
	}
	if !Stalled(run) {
		t.Fatal("the run must be stalled at the cap")
	}
	if !LastRed(run) {
		t.Fatal("the latest replay was red")
	}
	if got := Observe(run, 0, 0, true); got != 0 {
		t.Fatalf("a green full run clears the stalls, got %d", got)
	}
	if Stalled(run) || LastRed(run) {
		t.Fatal("a green full run clears stalled and red")
	}
	if got := Left(run); got != Max-2 {
		t.Fatalf("the replays stay spent after a green: left = %d, want %d", got, Max-2)
	}
}
