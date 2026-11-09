package admin

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// The per-task cost overlay windows build-stage call cost onto task-completion
// events. This pins the three properties that matter: a batched (same-second)
// multi-complete splits its window evenly and is flagged estimated, a solo
// completion owns its whole window, and Σ(tasks) + residual reconciles exactly
// to Σ(calls) - so the overlay can never invent or lose tokens.
func TestAttributeBuildCost_WindowsSplitsAndReconciles(t *testing.T) {
	comps := []taskCompletion{
		{TaskID: "t1", Title: "One", TS: "2026-01-01 00:00:10", Iteration: 2},
		{TaskID: "t2", Title: "Two", TS: "2026-01-01 00:00:10", Iteration: 2}, // same second -> K=2 batch
		{TaskID: "t3", Title: "Three", TS: "2026-01-01 00:00:30", Iteration: 5},
	}
	calls := []buildCall{
		{Effective: 100, Output: 10, DurMs: 1000, TS: "2026-01-01 00:00:05"},
		{Effective: 100, Output: 10, DurMs: 1000, TS: "2026-01-01 00:00:10"},
		{Effective: 100, Output: 10, DurMs: 1000, TS: "2026-01-01 00:00:20"},
		{Effective: 100, Output: 10, DurMs: 1000, TS: "2026-01-01 00:00:30"},
		{Effective: 100, Output: 10, DurMs: 1000, TS: "2026-01-01 00:00:40"}, // after last completion -> residual
	}

	rows, residual := attributeBuildCost(comps, calls)
	if len(rows) != 3 {
		t.Fatalf("want 3 task rows, got %d", len(rows))
	}
	byID := map[string]taskCost{}
	for _, r := range rows {
		byID[r.TaskID] = r
	}

	// Group1 window = calls with ts <= 00:00:10 -> 2 calls, eff 200, split /2.
	if byID["t1"].Effective != 100 || byID["t2"].Effective != 100 {
		t.Errorf("t1/t2 effective = %d/%d, want 100/100", byID["t1"].Effective, byID["t2"].Effective)
	}
	if !byID["t1"].Estimated || !byID["t2"].Estimated {
		t.Errorf("batched completions t1/t2 must be flagged estimated")
	}
	if byID["t1"].Calls+byID["t2"].Calls != 2 {
		t.Errorf("t1+t2 calls = %d, want 2 (the window's 2 calls, split)", byID["t1"].Calls+byID["t2"].Calls)
	}

	// Group2 window = 00:00:10 < ts <= 00:00:30 -> 2 calls, sole owner t3.
	if byID["t3"].Effective != 200 || byID["t3"].Calls != 2 {
		t.Errorf("t3 effective/calls = %d/%d, want 200/2", byID["t3"].Effective, byID["t3"].Calls)
	}
	if byID["t3"].Estimated {
		t.Errorf("solo completion t3 must NOT be estimated")
	}

	// Residual = the trailing call after the last completion.
	if residual.Calls != 1 || residual.Effective != 100 {
		t.Errorf("residual calls/effective = %d/%d, want 1/100", residual.Calls, residual.Effective)
	}

	// Exact reconciliation: nothing invented, nothing lost.
	var sum int64
	for _, r := range rows {
		sum += r.Effective
	}
	sum += residual.Effective
	var total int64
	for _, c := range calls {
		total += c.Effective
	}
	if sum != total {
		t.Errorf("reconciliation failed: sum(tasks)+residual=%d, sum(calls)=%d", sum, total)
	}
}

// Empty completion list -> everything is unattributed residual, nothing lost.
// This is the historical-build / pre-instrumentation shape.
func TestAttributeBuildCost_NoCompletionsAllResidual(t *testing.T) {
	calls := []buildCall{
		{Effective: 100, Output: 10, TS: "2026-01-01 00:00:05"},
		{Effective: 200, Output: 20, TS: "2026-01-01 00:00:15"},
	}
	rows, residual := attributeBuildCost(nil, calls)
	if len(rows) != 0 {
		t.Errorf("want 0 task rows with no completions, got %d", len(rows))
	}
	if residual.Calls != 2 || residual.Effective != 300 {
		t.Errorf("residual = %d calls / %d eff, want 2 / 300", residual.Calls, residual.Effective)
	}
}

// End-to-end through buildTokenSummary: with of_run_log task events present, the
// summary gains a `tasks` array and the per-task + residual effective tokens
// reconcile to the build stage's effective input (openai-chat: read factor 0.20).
func TestBuildTokenSummary_IncludesPerTaskAttribution(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE of_llm_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source TEXT, run_id TEXT, stage TEXT, provider_type TEXT,
		provider_name TEXT DEFAULT '', model TEXT DEFAULT '',
		input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
		cache_read_input_tokens INTEGER DEFAULT 0, cache_creation_input_tokens INTEGER DEFAULT 0,
		stable_prefix_bytes INTEGER DEFAULT 0, total_request_bytes INTEGER DEFAULT 0,
		duration_ms INTEGER DEFAULT 0, created_at DATETIME
	)`); err != nil {
		t.Fatalf("create of_llm_calls: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE of_run_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT, kind TEXT, ref TEXT, payload TEXT DEFAULT '',
		input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
		duration_ms INTEGER DEFAULT 0, created_at DATETIME
	)`); err != nil {
		t.Fatalf("create of_run_log: %v", err)
	}

	seedCall := func(ts string, in, out, cacheRead, dur int) {
		if _, err := db.Exec(
			`INSERT INTO of_llm_calls (source, run_id, stage, provider_type, input_tokens, output_tokens, cache_read_input_tokens, duration_ms, created_at)
			 VALUES ('engine','run_1','build','openai-chat',?,?,?,?,?)`,
			in, out, cacheRead, dur, ts,
		); err != nil {
			t.Fatalf("seed call: %v", err)
		}
	}
	seedComplete := func(ts, id, payload string) {
		if _, err := db.Exec(
			`INSERT INTO of_run_log (run_id, kind, ref, payload, created_at) VALUES ('run_1','task',?,?,?)`,
			id, payload, ts,
		); err != nil {
			t.Fatalf("seed completion: %v", err)
		}
	}

	// build calls: @05 -> t1 window, @20 -> t2 window, @40 -> residual.
	seedCall("2026-01-01 00:00:05", 100, 10, 0, 1000)
	seedCall("2026-01-01 00:00:20", 100, 10, 0, 2000)
	seedCall("2026-01-01 00:00:40", 100, 10, 0, 3000)
	// A plan-stage call and a chat call must not leak into build attribution.
	if _, err := db.Exec(`INSERT INTO of_llm_calls (source, run_id, stage, provider_type, input_tokens, created_at) VALUES ('engine','run_1','plan','openai-chat',500,'2026-01-01 00:00:01')`); err != nil {
		t.Fatalf("seed plan call: %v", err)
	}
	seedComplete("2026-01-01 00:00:10", "t1", `{"iteration":1,"title":"First task"}`)
	seedComplete("2026-01-01 00:00:30", "t2", `{"iteration":3,"title":"Second task"}`)

	summary, err := buildTokenSummary(db, "run_1")
	if err != nil {
		t.Fatalf("buildTokenSummary: %v", err)
	}

	tasks, ok := summary["tasks"].([]map[string]any)
	if !ok || len(tasks) != 2 {
		t.Fatalf("want 2 per-task rows, got %+v", summary["tasks"])
	}
	if title, _ := tasks[0]["title"].(string); title != "First task" {
		t.Errorf("tasks[0].title = %q, want %q (from payload)", title, "First task")
	}
	resid, ok := summary["tasks_unattributed"].(map[string]any)
	if !ok {
		t.Fatalf("missing tasks_unattributed: %+v", summary)
	}

	// openai-chat read factor 0.20, cacheRead 0 => effective == input == 100 each.
	var sum int64
	for _, tr := range tasks {
		sum += tr["effective_input_tokens"].(int64)
	}
	sum += resid["effective_input_tokens"].(int64)
	if sum != 300 {
		t.Errorf("per-task + residual effective = %d, want 300 (build calls only, plan/chat excluded)", sum)
	}
	if rc, _ := resid["calls"].(int); rc != 1 {
		t.Errorf("residual calls = %v, want 1 (the @40 trailing call)", resid["calls"])
	}
}
