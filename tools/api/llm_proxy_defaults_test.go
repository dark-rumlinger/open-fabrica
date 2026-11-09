package api

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupLLMProxyDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	return db
}

func llmMaxTokens(t *testing.T, db dbh.Handle, path string) int {
	t.Helper()
	var mt int
	if err := db.QueryRow("SELECT max_tokens FROM of_llm_endpoints WHERE path = ?", path).Scan(&mt); err != nil {
		t.Fatalf("read max_tokens for %q: %v", path, err)
	}
	return mt
}

// TestLLMProxyCreate_MaxTokensStaysUnsetWhenOmitted pins the tag shape the
// schema-conditional default depends on: a default tag on max_tokens would
// populate the field before the handler runs, making "omitted" indistinguishable
// from "explicitly 4096" and silencing the response_schema floor below.
func TestLLMProxyCreate_MaxTokensStaysUnsetWhenOmitted(t *testing.T) {
	a := &LLMProxyCreate{}
	if err := tools.PopulateAction(a, map[string]any{
		"endpoint_path":   "chat",
		"response_schema": map[string]any{"type": "object"},
	}); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if a.MaxTokens != 0 {
		t.Fatalf("MaxTokens populated to %d with the arg omitted; a default tag on max_tokens defeats the handler's schema-conditional floor", a.MaxTokens)
	}
}

// TestLLMProxyCreate_SchemaRaisesMaxTokensFloor: an endpoint constrained to a
// response schema wraps its reply in JSON, so truncation makes it unparseable -
// the default response budget must be the higher floor.
func TestLLMProxyCreate_SchemaRaisesMaxTokensFloor(t *testing.T) {
	db := setupLLMProxyDB(t)
	ctx := &tools.ToolContext{DB: db}

	a := &LLMProxyCreate{
		EndpointPath:   "generate",
		ResponseSchema: map[string]any{"type": "object"},
	}
	res, err := a.Run(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Success {
		t.Fatalf("create failed: %s", res.Error)
	}
	if got := llmMaxTokens(t, db, "generate"); got != 8192 {
		t.Fatalf("max_tokens with response_schema and no explicit cap = %d, want 8192", got)
	}
}

// TestLLMProxyCreate_DefaultMaxTokensWithoutSchema: free-form endpoints keep the
// plain default.
func TestLLMProxyCreate_DefaultMaxTokensWithoutSchema(t *testing.T) {
	db := setupLLMProxyDB(t)
	ctx := &tools.ToolContext{DB: db}

	// Path is "assistant", not "chat": the runtime serves an LLM proxy at
	// /api/<path>/chat, so naming the path "chat" is rejected by the spine claim.
	a := &LLMProxyCreate{EndpointPath: "assistant"}
	if _, err := a.Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := llmMaxTokens(t, db, "assistant"); got != 4096 {
		t.Fatalf("max_tokens without schema = %d, want 4096", got)
	}
}

// TestLLMProxyCreate_ExplicitMaxTokensWins: a caller-supplied cap overrides both
// floors, schema or not.
func TestLLMProxyCreate_ExplicitMaxTokensWins(t *testing.T) {
	db := setupLLMProxyDB(t)
	ctx := &tools.ToolContext{DB: db}

	a := &LLMProxyCreate{
		EndpointPath:   "generate",
		MaxTokens:      2000,
		ResponseSchema: map[string]any{"type": "object"},
	}
	if _, err := a.Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := llmMaxTokens(t, db, "generate"); got != 2000 {
		t.Fatalf("max_tokens = %d, want the caller's 2000", got)
	}
}

// TestLLMProxyBudgetRejectsNegative pins the daily_token_budget min:"0" bound on
// both create and update: a cost guard must never read a negative as unlimited.
// The reflect layer rejects it before dispatch; update also proves the bound is
// enforced through the *int tri-state field.
func TestLLMProxyBudgetRejectsNegative(t *testing.T) {
	if err := tools.PopulateAction(&LLMProxyCreate{}, map[string]any{
		"endpoint_path":      "chat",
		"daily_token_budget": -1,
	}); err == nil {
		t.Fatal("create accepted a negative daily_token_budget; min:\"0\" must reject it")
	}
	if err := tools.PopulateAction(&LLMProxyUpdate{}, map[string]any{
		"endpoint_path":      "chat",
		"daily_token_budget": -1,
	}); err == nil {
		t.Fatal("update accepted a negative daily_token_budget; min:\"0\" must reject it on the *int field")
	}
	if err := tools.PopulateAction(&LLMProxyCreate{}, map[string]any{
		"endpoint_path":      "chat",
		"daily_token_budget": 0,
	}); err != nil {
		t.Fatalf("create rejected a zero (unlimited) budget: %v", err)
	}
}
