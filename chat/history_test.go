package chat

import (
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

// setupTimelineDB creates the minimal project-DB tables LoadTimeline reads:
// of_chat_messages (history), of_runs (cursor), and of_run_log (stage
// transitions).
func setupTimelineDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Pin to one connection: each :memory: connection is its own database, and
	// LoadTimeline keeps a Rows open while issuing further queries, which would
	// otherwise open a second (empty) connection mid-call.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE of_chat_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT, role TEXT, content TEXT,
			tool_calls TEXT, tool_call_id TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_runs (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL DEFAULT 'build',
			status TEXT NOT NULL DEFAULT 'running',
			stage TEXT NOT NULL DEFAULT 'plan',
			plan_json TEXT,
			expanded_brief TEXT NOT NULL DEFAULT '',
			pause_reason TEXT,
			started_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_run_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			ref TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

// seedStage writes a kind='stage' run-log row with the engine's payload shape.
func seedStage(t *testing.T, db *sql.DB, runID, stage, status, ts string) {
	t.Helper()
	payload := fmt.Sprintf(`{"status":%q}`, status)
	if _, err := db.Exec(
		`INSERT INTO of_run_log (run_id, kind, ref, payload, duration_ms, created_at)
		 VALUES (?, 'stage', ?, ?, 1500, ?)`,
		runID, stage, payload, ts,
	); err != nil {
		t.Fatalf("seed stage %s/%s: %v", runID, stage, err)
	}
}

func seedMsg(t *testing.T, db *sql.DB, session, role, content, ts string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO of_chat_messages (session_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
		session, role, content, ts,
	); err != nil {
		t.Fatalf("seed msg: %v", err)
	}
}

func hasStage(stages []string, want string) bool {
	for _, s := range stages {
		if s == want {
			return true
		}
	}
	return false
}

// A completed fresh build: stage transitions replay from of_run_log (lowercase
// refs upper-cased for the UI), the cursor comes from the latest of_runs row,
// and only completed/failed transitions surface (not the "started" rows).
func TestLoadTimeline_CompletedBuild(t *testing.T) {
	db := setupTimelineDB(t)
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, kind, stage, status, expanded_brief, started_at)
		 VALUES ('run_1','build','live','completed','A polished tip calc.','2026-06-11 10:00:00')`,
	); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	for _, s := range []struct{ stage, status, ts string }{
		{"expand", "started", "2026-06-11 10:00:01"},
		{"expand", "completed", "2026-06-11 10:00:02"},
		{"plan", "started", "2026-06-11 10:00:03"},
		{"plan", "completed", "2026-06-11 10:00:04"},
		{"build", "started", "2026-06-11 10:00:05"},
		{"build", "completed", "2026-06-11 10:00:06"},
	} {
		seedStage(t, db, "run_1", s.stage, s.status, s.ts)
	}
	seedMsg(t, db, "brain", "assistant", "Starting your build", "2026-06-11 10:00:00")
	seedMsg(t, db, "admin", "user", "looks good", "2026-06-11 10:00:07")

	tl, err := LoadTimeline(db, 50)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}

	// Only completed/failed transitions, upper-cased, with duration carried.
	if len(tl.Stages) != 3 {
		t.Fatalf("want 3 stage transitions (completed only), got %d: %+v", len(tl.Stages), tl.Stages)
	}
	for _, s := range tl.Stages {
		if s.Status != "completed" && s.Status != "failed" {
			t.Errorf("started rows must be filtered out, got status %q", s.Status)
		}
		if s.Stage != "EXPAND" && s.Stage != "PLAN" && s.Stage != "BUILD" {
			t.Errorf("stage name not upper-cased: %q", s.Stage)
		}
		if s.DurationMS != 1500 {
			t.Errorf("duration not carried: %d", s.DurationMS)
		}
	}

	if tl.Pipeline == nil {
		t.Fatal("expected a pipeline snapshot")
	}
	if tl.Pipeline.Stage != "LIVE" {
		t.Errorf("pipeline stage = %q, want LIVE", tl.Pipeline.Stage)
	}
	if tl.Pipeline.Paused {
		t.Error("completed run must not report paused")
	}
	for _, want := range []string{"EXPAND", "PLAN", "BUILD"} {
		if !hasStage(tl.Pipeline.CompletedStages, want) {
			t.Errorf("completed_stages missing %s: %v", want, tl.Pipeline.CompletedStages)
		}
	}
}

// A paused run surfaces its pause reason and current stage so the chat UI can
// light the tracker's error state on reload.
func TestLoadTimeline_PausedRun(t *testing.T) {
	db := setupTimelineDB(t)
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, kind, stage, status, pause_reason, started_at)
		 VALUES ('run_1','build','build','paused','awaiting_owner_answers','2026-06-11 10:00:00')`,
	); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	seedStage(t, db, "run_1", "plan", "completed", "2026-06-11 10:00:02")

	tl, err := LoadTimeline(db, 50)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if tl.Pipeline == nil {
		t.Fatal("expected a pipeline snapshot")
	}
	if !tl.Pipeline.Paused {
		t.Error("paused run must report paused")
	}
	if tl.Pipeline.Stage != "BUILD" {
		t.Errorf("pipeline stage = %q, want BUILD", tl.Pipeline.Stage)
	}
	if tl.Pipeline.PauseReason != "awaiting_owner_answers" {
		t.Errorf("pause reason = %q", tl.Pipeline.PauseReason)
	}
}

// EXPAND is project-scoped: a later update run that skips EXPAND must still
// show it as completed, because the expanded_brief persists from the first
// build. This guards the cross-run brief lookup that reads expanded_brief from
// of_runs.
func TestLoadTimeline_ExpandProjectScoped(t *testing.T) {
	db := setupTimelineDB(t)
	// First build ran EXPAND; brief persisted on run_1.
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, kind, stage, status, expanded_brief, started_at)
		 VALUES ('run_1','build','live','completed','the brief','2026-06-11 10:00:00')`,
	); err != nil {
		t.Fatalf("insert run_1: %v", err)
	}
	seedStage(t, db, "run_1", "expand", "completed", "2026-06-11 10:00:01")
	// Later update run is the latest; it skipped EXPAND and carries no brief.
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, kind, stage, status, expanded_brief, started_at)
		 VALUES ('run_2','update','live','completed','','2026-06-11 11:00:00')`,
	); err != nil {
		t.Fatalf("insert run_2: %v", err)
	}
	seedStage(t, db, "run_2", "plan", "completed", "2026-06-11 11:00:01")
	seedStage(t, db, "run_2", "build", "completed", "2026-06-11 11:00:02")

	tl, err := LoadTimeline(db, 50)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if tl.Pipeline == nil {
		t.Fatal("expected a pipeline snapshot")
	}
	// EXPAND comes from run_1's brief even though the latest run (run_2) logged
	// no expand transition, and it leads the list.
	if len(tl.Pipeline.CompletedStages) == 0 || tl.Pipeline.CompletedStages[0] != "EXPAND" {
		t.Errorf("EXPAND must lead completed_stages: %v", tl.Pipeline.CompletedStages)
	}
	for _, want := range []string{"EXPAND", "PLAN", "BUILD"} {
		if !hasStage(tl.Pipeline.CompletedStages, want) {
			t.Errorf("completed_stages missing %s: %v", want, tl.Pipeline.CompletedStages)
		}
	}
}

// No runs yet (fresh project): LoadTimeline returns messages with no pipeline
// snapshot rather than erroring on the missing run.
func TestLoadTimeline_NoRuns(t *testing.T) {
	db := setupTimelineDB(t)
	seedMsg(t, db, "admin", "user", "hello", "2026-06-11 10:00:00")

	tl, err := LoadTimeline(db, 50)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if tl.Pipeline != nil {
		t.Errorf("no runs should yield no pipeline snapshot, got %+v", tl.Pipeline)
	}
	if len(tl.Messages) != 1 {
		t.Errorf("expected the seeded message, got %d", len(tl.Messages))
	}
}
