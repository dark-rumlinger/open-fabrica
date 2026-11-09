package api

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupFunctionDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	return db
}

func functionRateLimit(t *testing.T, db dbh.Handle, path string) int {
	t.Helper()
	var rl int
	if err := db.QueryRow("SELECT rate_limit FROM of_function_endpoints WHERE path = ?", path).Scan(&rl); err != nil {
		t.Fatalf("read rate_limit for %q: %v", path, err)
	}
	return rl
}

// TestFunctionCreate_DefaultsRateLimit pins the create-side half of the
// dead-on-arrival defect. The handler inserts rate_limit explicitly, so the
// column's DEFAULT never applies and an omitted arg would persist 0 - which the
// limiter reads as "reject everything", not "unlimited".
func TestFunctionCreate_DefaultsRateLimit(t *testing.T) {
	db := setupFunctionDB(t)
	ctx := &tools.ToolContext{DB: db}

	a := &FunctionCreate{
		EndpointPath: "quote",
		Code:         `function handler(req, ctx) { return { ok: true }; }`,
	}
	res, err := a.Run(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Success {
		t.Fatalf("create failed: %s", res.Error)
	}
	if got := functionRateLimit(t, db, "quote"); got != defaultFunctionRateLimit {
		t.Fatalf("rate_limit with the arg omitted = %d, want %d (0 would 429 the first request)", got, defaultFunctionRateLimit)
	}
}

// TestFunctionCreate_KeepsExplicitRateLimit is the counterweight: defaulting an
// omitted value must not overwrite one the caller set.
func TestFunctionCreate_KeepsExplicitRateLimit(t *testing.T) {
	db := setupFunctionDB(t)
	ctx := &tools.ToolContext{DB: db}

	a := &FunctionCreate{
		EndpointPath: "quote",
		Code:         `function handler(req, ctx) { return { ok: true }; }`,
		RateLimit:    5,
	}
	if _, err := a.Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := functionRateLimit(t, db, "quote"); got != 5 {
		t.Fatalf("rate_limit = %d, want the caller's 5", got)
	}
}
