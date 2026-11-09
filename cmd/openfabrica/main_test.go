package main

import (
	"path/filepath"
	"testing"

	"openfabrica/config"
	"openfabrica/db"
)

// TestPermissiveDefaultIssues pins the startup-nag decision: silent on a
// local-only instance, and one issue per wide-open public default once a domain
// makes the instance internet-reachable.
func TestPermissiveDefaultIssues(t *testing.T) {
	strict := "default-src 'self'; script-src 'self' {nonce}; object-src 'none'"

	cases := []struct {
		name        string
		hasDomains  bool
		corsOrigins []string
		csp         string
		wantCount   int
	}{
		{"local-only stays silent even with open defaults", false, nil, config.DefaultPublicCSP, 0},
		{"exposed with both defaults open", true, nil, config.DefaultPublicCSP, 2},
		{"exposed, CORS set, CSP still permissive", true, []string{"https://app.example.com"}, config.DefaultPublicCSP, 1},
		{"exposed, CSP tightened, CORS still wildcard", true, nil, strict, 1},
		{"exposed but both tightened", true, []string{"https://app.example.com"}, strict, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := permissiveDefaultIssues(c.hasDomains, c.corsOrigins, c.csp)
			if len(got) != c.wantCount {
				t.Errorf("permissiveDefaultIssues() = %v (%d issues), want %d", got, len(got), c.wantCount)
			}
		})
	}
}

// TestResolveEngineDailyTokenBudget pins the precedence the spend guardrail
// depends on: env var > admin/wizard DB setting > config (config.json value or
// the safe built-in default).
func TestResolveEngineDailyTokenBudget(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{EngineDailyTokenBudget: config.DefaultEngineDailyTokenBudget}

	// 1. No env, no DB setting -> falls back to the config/default value.
	if got := resolveEngineDailyTokenBudget(cfg, database); got != config.DefaultEngineDailyTokenBudget {
		t.Fatalf("default fallback: got %d, want %d", got, config.DefaultEngineDailyTokenBudget)
	}

	// 2. DB setting present -> wins over the config value (no env).
	if err := database.SetSetting("engine_daily_token_budget", "250000"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	if got := resolveEngineDailyTokenBudget(cfg, database); got != 250000 {
		t.Fatalf("db setting precedence: got %d, want 250000", got)
	}

	// An explicit 0 in the DB means unlimited and must be honored, not skipped.
	if err := database.SetSetting("engine_daily_token_budget", "0"); err != nil {
		t.Fatalf("set setting 0: %v", err)
	}
	if got := resolveEngineDailyTokenBudget(cfg, database); got != 0 {
		t.Fatalf("db setting 0 (unlimited): got %d, want 0", got)
	}

	// 3. Env var set -> wins over both the DB setting and config.
	t.Setenv("OPENFABRICA_ENGINE_DAILY_TOKEN_BUDGET", "999")
	if got := resolveEngineDailyTokenBudget(cfg, database); got != 999 {
		t.Fatalf("env precedence: got %d, want 999", got)
	}
}
