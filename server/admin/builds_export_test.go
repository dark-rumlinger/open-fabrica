package admin

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// exportWindowSchema mirrors production shape for the columns the export's chat
// window touches: of_runs.started_at/completed_at and of_chat_messages.created_at
// are DATETIME (so the sqlite driver reformats bare reads to RFC3339), and rows are
// seeded space-separated the way CURRENT_TIMESTAMP writes them. The existing
// triage_test.go schema declares these TEXT and seeds RFC3339, which HIDES the
// mismatch this test exists to catch.
const exportWindowSchema = `
CREATE TABLE of_runs (
  id TEXT PRIMARY KEY, status TEXT, stage TEXT,
  started_at DATETIME, completed_at DATETIME,
  engine_version TEXT NOT NULL DEFAULT '', engine_built_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE of_llm_calls (
  id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT, source TEXT,
  input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0
);
CREATE TABLE of_chat_messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
  content TEXT, tool_calls TEXT, tool_call_id TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

// TestRunChatMessages_SpaceFormatWindow verifies the build export windows its
// whole chat transcript when of_runs timestamps are stored space-separated. Those
// timestamps read back (bare DATETIME) as RFC3339 bounds via loadBuildSummary and
// are then used to window space-separated of_chat_messages.created_at: a raw
// `created_at >= ?` comparison returns zero rows here, while the datetime()-
// normalized query in runChatMessages returns the transcript.
func TestRunChatMessages_SpaceFormatWindow(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(exportWindowSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// Run window written space-separated (as SQLite CURRENT_TIMESTAMP does).
	if _, err := db.Exec(
		`INSERT INTO of_runs (id, status, stage, started_at, completed_at)
		 VALUES ('run_x','completed','live','2026-07-04 13:32:34','2026-07-04 13:44:47')`,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	seedMsg := func(role, createdAt string) {
		if _, err := db.Exec(
			`INSERT INTO of_chat_messages (session_id, role, content, created_at)
			 VALUES ('brain', ?, 'x', ?)`, role, createdAt); err != nil {
			t.Fatalf("seed msg: %v", err)
		}
	}
	seedMsg("assistant", "2026-07-04 13:35:00") // in window
	seedMsg("tool", "2026-07-04 13:40:00")      // in window
	seedMsg("assistant", "2026-07-04 20:00:00") // AFTER completed_at - must be excluded
	// A message from a different session must never appear.
	if _, err := db.Exec(
		`INSERT INTO of_chat_messages (session_id, role, content, created_at)
		 VALUES ('other','assistant','x','2026-07-04 13:36:00')`); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	build, err := loadBuildSummary(db, "run_x")
	if err != nil {
		t.Fatalf("loadBuildSummary: %v", err)
	}
	// Document the driver behavior this depends on: a bare DATETIME column read
	// into a Go string comes back RFC3339 (has a 'T'), NOT the stored space form.
	if !strings.Contains(build.StartedAt, "T") {
		t.Fatalf("expected loadBuildSummary to return an RFC3339 started_at (with 'T'); got %q - "+
			"driver no longer reformats DATETIME, revisit whether the datetime() wrap is still needed", build.StartedAt)
	}

	// Negative control: the raw (non-datetime) comparison drops every in-window row
	// because the RFC3339 bound sorts after every space-separated created_at.
	var rawCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM of_chat_messages WHERE session_id='brain' AND created_at >= ?`,
		build.StartedAt).Scan(&rawCount); err != nil {
		t.Fatalf("raw count: %v", err)
	}
	if rawCount != 0 {
		t.Errorf("raw (non-datetime) window matched %d rows; expected 0 - the raw-comparison premise no longer holds", rawCount)
	}

	// datetime()-normalized window returns exactly the two in-window brain
	// messages and excludes the out-of-window one and the other session.
	msgs, err := runChatMessages(db, build.StartedAt, build.CompletedAt)
	if err != nil {
		t.Fatalf("runChatMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("runChatMessages returned %d messages, want 2 (transcript dropped or over-included): %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "assistant" || msgs[1]["role"] != "tool" {
		t.Errorf("unexpected message ordering/content: %+v", msgs)
	}
}
