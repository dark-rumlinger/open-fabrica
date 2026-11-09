package admin

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// The Builds cost panel (web/admin builds.js renderCostPanel) reads the run-wide
// figures from a nested `totals` object and returns early when `totals.calls`
// is missing - so buildTokenSummary MUST nest the totals, not flatten them to
// the top level, or the whole panel (token usage + cache %) silently vanishes.
// This pins that contract.
func TestBuildTokenSummary_NestsTotalsForCostPanel(t *testing.T) {
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
		cache_read_input_tokens INTEGER DEFAULT 0,
		cache_creation_input_tokens INTEGER DEFAULT 0,
		stable_prefix_bytes INTEGER DEFAULT 0,
		total_request_bytes INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_llm_calls: %v", err)
	}
	seed := func(stage, providerType string, in, out, cacheRead, cacheCreate int) {
		if _, err := db.Exec(
			`INSERT INTO of_llm_calls
			 (source, run_id, stage, provider_type, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens)
			 VALUES ('engine','run_1',?,?,?,?,?,?)`,
			stage, providerType, in, out, cacheRead, cacheCreate,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed("plan", "anthropic", 1000, 200, 800, 100)
	seed("build", "anthropic", 2000, 500, 1500, 200)
	seed("build", "openai-chat", 1000, 300, 900, 0)
	// A chat-source call (and a different run) must not leak into the engine totals.
	if _, err := db.Exec(`INSERT INTO of_llm_calls (source, run_id, stage, input_tokens) VALUES ('chat','run_1','',9999)`); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	summary, err := buildTokenSummary(db, "run_1")
	if err != nil {
		t.Fatalf("buildTokenSummary: %v", err)
	}

	// Regression guard: totals must be nested, never flat at the top level.
	if _, flat := summary["total_calls"]; flat {
		t.Error("totals must be nested under `totals`, not flattened - the cost panel reads data.totals")
	}
	totals, ok := summary["totals"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing nested `totals` object: %+v", summary)
	}
	if calls, _ := totals["calls"].(int64); calls != 3 {
		t.Errorf("totals.calls = %v, want 3 (engine only; chat excluded)", totals["calls"])
	}
	// billed = input + cache_read + cache_creation = 4000 + 3200 + 300
	if billed, _ := totals["billed_input_tokens"].(int64); billed != 7500 {
		t.Errorf("totals.billed_input_tokens = %v, want 7500", totals["billed_input_tokens"])
	}
	if out, _ := totals["output_tokens"].(int64); out != 1000 {
		t.Errorf("totals.output_tokens = %v, want 1000", totals["output_tokens"])
	}
	// cache_hit_ratio = cache_read / billed = 3200/7500 ≈ 0.427
	if r, _ := totals["cache_hit_ratio"].(float64); r < 0.42 || r > 0.43 {
		t.Errorf("totals.cache_hit_ratio = %v, want ~0.427", totals["cache_hit_ratio"])
	}
	// Effective input prices the cache per provider type:
	//   plan/anthropic:   1000 + 800*0.10 + 100*1.25 = 1205
	//   build/anthropic:  2000 + 1500*0.10 + 200*1.25 = 2400
	//   build/openai:     1000 + 900*0.20 + 0 = 1180
	if eff, _ := totals["effective_input_tokens"].(int64); eff != 4785 {
		t.Errorf("totals.effective_input_tokens = %v, want 4785", totals["effective_input_tokens"])
	}
	stages, ok := summary["stages"].([]map[string]any)
	if !ok || len(stages) != 2 {
		t.Fatalf("expected 2 per-stage rows (plan, build), got %+v", summary["stages"])
	}
}
