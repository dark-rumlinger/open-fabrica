package llm

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestLLMCallsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_llm_calls (
		source TEXT,
		provider_type TEXT,
		provider_name TEXT,
		model TEXT,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		cache_read_input_tokens INTEGER DEFAULT 0,
		cache_creation_input_tokens INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// insertCall adds one of_llm_calls row. whenExpr is a SQL datetime expression
// (e.g. "datetime('now')" or "datetime('now','-2 days')") so tests control the
// day without depending on wall-clock time in Go.
func insertCall(t *testing.T, db *sql.DB, source string, in, out int, whenExpr string) {
	t.Helper()
	if _, err := db.Exec(
		"INSERT INTO of_llm_calls (source, provider_type, input_tokens, output_tokens, created_at) VALUES (?, '', ?, ?, "+whenExpr+")",
		source, in, out,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// TestTokensUsedTodayForSources pins the two filters the budget depends on: only
// the requested sources count, and only today's rows.
func TestTokensUsedTodayForSources(t *testing.T) {
	db := newTestLLMCallsDB(t)

	insertCall(t, db, "engine", 100, 50, "datetime('now')")              // today, counted
	insertCall(t, db, "trigger", 10, 5, "datetime('now')")               // today, counted
	insertCall(t, db, "job", 20, 0, "datetime('now')")                   // today, counted
	insertCall(t, db, "function", 1, 1, "datetime('now')")               // today, counted
	insertCall(t, db, "chat", 1000, 1000, "datetime('now')")             // excluded source
	insertCall(t, db, "engine", 9999, 9999, "datetime('now','-2 days')") // excluded: not today

	got, err := TokensUsedTodayForSources(db, UnattendedSources)
	if err != nil {
		t.Fatalf("tally: %v", err)
	}
	want := int64(100 + 50 + 10 + 5 + 20 + 1 + 1) // 187; chat + old rows excluded
	if got != want {
		t.Errorf("TokensUsedTodayForSources = %d, want %d", got, want)
	}
}

// TestDailyBudgetExceeded pins the threshold and the unlimited (0) case.
func TestDailyBudgetExceeded(t *testing.T) {
	db := newTestLLMCallsDB(t)
	insertCall(t, db, "engine", 300, 200, "datetime('now')") // 500 today

	cases := []struct {
		budget       int
		wantExceeded bool
	}{
		{0, false},    // 0 = unlimited, never exceeded
		{1000, false}, // under
		{501, false},  // just under
		{500, true},   // at the ceiling
		{400, true},   // over
	}
	for _, c := range cases {
		exceeded, used, err := DailyBudgetExceeded(db, c.budget)
		if err != nil {
			t.Fatalf("budget %d: %v", c.budget, err)
		}
		if exceeded != c.wantExceeded {
			t.Errorf("DailyBudgetExceeded(budget=%d) exceeded=%v (used=%d), want %v", c.budget, exceeded, used, c.wantExceeded)
		}
	}
}
