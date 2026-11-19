package main

import (
	"path/filepath"
	"testing"

	"openfabrica/config"
	"openfabrica/db"
)

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
