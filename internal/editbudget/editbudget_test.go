package editbudget

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// editFailuresDDL matches the of_edit_failures table in db/project_migrations/001_initial.sql.
const editFailuresDDL = `CREATE TABLE IF NOT EXISTS of_edit_failures (
    run_id     TEXT NOT NULL,
    category   TEXT NOT NULL,
    key        TEXT NOT NULL,
    count      INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, category, key)
)`

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one connection keeps the write transaction and reads consistent
	if _, err := db.Exec(editFailuresDDL); err != nil {
		t.Fatal(err)
	}
	return db
}

// The count lives in the DB, so a stuck edit loop is caught across the build->repair
// restart the in-memory ReadTracker reset on: reopening the file (a fresh process)
// keeps the count, where the in-memory counter would be empty and hand out a fresh
// allowance.
func TestRecordSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	db := openDB(t, path)
	for i := 1; i <= 3; i++ {
		if got := Record(db, "run_a", NoOp, "file\x00app.js"); got != i {
			t.Fatalf("record %d: count = %d, want %d", i, got, i)
		}
	}
	db.Close()

	// Simulated restart: a fresh handle to the same file keeps the count.
	db2 := openDB(t, path)
	defer db2.Close()
	if got := Record(db2, "run_a", NoOp, "file\x00app.js"); got != 4 {
		t.Fatalf("count after restart = %d, want 4", got)
	}
	// A different run gets its own count.
	if got := Record(db2, "run_b", NoOp, "file\x00app.js"); got != 1 {
		t.Fatalf("other run count = %d, want 1", got)
	}
}

// RunTotal catches the distributed shape: the same mistake spread over many files,
// none of which reaches the per-file cap on its own.
func TestRunTotalSumsAcrossKeys(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "p.db"))
	defer db.Close()
	Record(db, "run_a", NoOp, "file\x00a.js")
	Record(db, "run_a", NoOp, "file\x00b.js")
	Record(db, "run_a", NoOp, "file\x00c.js")
	// A different category must not count toward the no-op total.
	Record(db, "run_a", Anchor, "file\x00d.js")
	if got := RunTotal(db, "run_a", NoOp); got != 3 {
		t.Fatalf("noop run total = %d, want 3", got)
	}
	if got := RunTotal(db, "run_a", Anchor); got != 1 {
		t.Fatalf("anchor run total = %d, want 1", got)
	}
}

// Clear resets a resolved streak (a successful write) without touching other keys.
func TestClearResetsOneKey(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "p.db"))
	defer db.Close()
	Record(db, "run_a", Anchor, "file\x00a.js")
	Record(db, "run_a", Anchor, "file\x00a.js")
	Record(db, "run_a", Anchor, "file\x00b.js")
	Clear(db, "run_a", Anchor, "file\x00a.js")
	if got := Record(db, "run_a", Anchor, "file\x00a.js"); got != 1 {
		t.Fatalf("count after clear = %d, want 1 (streak reset)", got)
	}
	if got := Record(db, "run_a", Anchor, "file\x00b.js"); got != 2 {
		t.Fatalf("untouched key = %d, want 2", got)
	}
}

// Every operation fails open on a nil handle or an empty run id, so bookkeeping never
// wedges a build and an off-run call (LIVE, tests) falls back to the in-memory path.
func TestFailsOpen(t *testing.T) {
	if got := Record(nil, "run_a", NoOp, "k"); got != 0 {
		t.Fatalf("nil db record = %d, want 0", got)
	}
	db := openDB(t, filepath.Join(t.TempDir(), "p.db"))
	defer db.Close()
	if got := Record(db, "", NoOp, "k"); got != 0 {
		t.Fatalf("empty run id record = %d, want 0", got)
	}
	if got := RunTotal(db, "", NoOp); got != 0 {
		t.Fatalf("empty run id total = %d, want 0", got)
	}
}
