package llm

import (
	"database/sql"
	"testing"
)

func spendDB(t *testing.T) *sql.DB {
	t.Helper()
	db := newTestLLMCallsDB(t)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE of_spend_limits (id INTEGER PRIMARY KEY CHECK (id = 1), daily_token_budget INTEGER, daily_public_token_budget INTEGER, daily_usd_cap REAL, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertPriced(t *testing.T, db *sql.DB, source, provider, model string, in, out int) {
	t.Helper()
	if _, err := db.Exec(
		"INSERT INTO of_llm_calls (source, provider_type, provider_name, model, input_tokens, output_tokens) VALUES (?, 'openai-chat', ?, ?, ?, ?)",
		source, provider, model, in, out); err != nil {
		t.Fatal(err)
	}
}

func TestSpendLimitsRoundTripAndMissingTable(t *testing.T) {
	// A database with no limits table (an older one) means no overrides.
	if l := LoadSpendLimits(newTestLLMCallsDB(t)); l.DailyTokenBudget != nil || l.DailyUSDCap != nil {
		t.Fatalf("no table must mean no overrides, got %+v", l)
	}
	db := spendDB(t)
	tokens, usd := 5000, 2.5
	if err := SaveSpendLimits(db, SpendLimits{DailyTokenBudget: &tokens, DailyUSDCap: &usd}); err != nil {
		t.Fatal(err)
	}
	l := LoadSpendLimits(db)
	if l.DailyTokenBudget == nil || *l.DailyTokenBudget != 5000 || l.DailyUSDCap == nil || *l.DailyUSDCap != 2.5 {
		t.Fatalf("round trip: %+v", l)
	}
	if err := SaveSpendLimits(db, SpendLimits{}); err != nil {
		t.Fatal(err)
	}
	if l := LoadSpendLimits(db); l.DailyTokenBudget != nil || l.DailyUSDCap != nil {
		t.Fatalf("clearing must remove both overrides, got %+v", l)
	}
}

func TestCheckSpendProjectTokenBudgetOverridesTheInstanceDefault(t *testing.T) {
	db := spendDB(t)
	insertCall(t, db, "engine", 600, 600, "datetime('now')")

	// The instance default is generous: not exceeded.
	if st := CheckSpend(db, 1_000_000); st.Exceeded {
		t.Fatalf("under the instance default: %+v", st)
	}
	// The project's own, tighter budget wins.
	tight := 1000
	_ = SaveSpendLimits(db, SpendLimits{DailyTokenBudget: &tight})
	if st := CheckSpend(db, 1_000_000); !st.Exceeded || st.TokenBudget != 1000 {
		t.Fatalf("project budget not applied: %+v", st)
	}
	// A project can also lift the cap entirely when the default is low.
	zero := 0
	_ = SaveSpendLimits(db, SpendLimits{DailyTokenBudget: &zero})
	if st := CheckSpend(db, 100); st.Exceeded {
		t.Fatalf("a project budget of 0 means unlimited: %+v", st)
	}
}

func TestCheckSpendDollarCap(t *testing.T) {
	t.Cleanup(Caps.Reset)
	db := spendDB(t)
	Caps.DeclareCost(ModelKey{Provider: "p", Model: "priced"}, ModelCost{Input: 10, Output: 30}) // USD per million tokens
	// 100k in + 50k out = $1.00 + $1.50 = $2.50
	insertPriced(t, db, "engine", "p", "priced", 100_000, 50_000)
	// Interactive chat is not unattended spend.
	insertPriced(t, db, "chat", "p", "priced", 10_000_000, 10_000_000)

	cap2 := 2.0
	_ = SaveSpendLimits(db, SpendLimits{DailyUSDCap: &cap2})
	st := CheckSpend(db, 0)
	if !st.Exceeded || st.Reason != "the daily AI spending cap" {
		t.Fatalf("a $2 cap with $2.50 spent: %+v", st)
	}
	if st.USDUsed < 2.49 || st.USDUsed > 2.51 {
		t.Errorf("USDUsed = %v, want about 2.50", st.USDUsed)
	}
	cap5 := 5.0
	_ = SaveSpendLimits(db, SpendLimits{DailyUSDCap: &cap5})
	if st := CheckSpend(db, 0); st.Exceeded {
		t.Fatalf("a $5 cap with $2.50 spent: %+v", st)
	}
}

func TestCheckSpendCountsCallsItCannotPrice(t *testing.T) {
	t.Cleanup(Caps.Reset)
	db := spendDB(t)
	insertPriced(t, db, "engine", "private", "unpriced-model", 1000, 1000)
	cap1 := 1.0
	_ = SaveSpendLimits(db, SpendLimits{DailyUSDCap: &cap1})
	st := CheckSpend(db, 0)
	if st.Exceeded || st.UnpricedCalls != 1 {
		t.Fatalf("an unpriced call cannot trip a dollar cap but must be reported: %+v", st)
	}
}

// TestPublicPoolIsSeparateAndTheDollarCapIsShared pins the two-pool split: end-user
// traffic (proxy, agent) draws on its own token budget without touching the build's,
// while the dollar cap is one ceiling over every billable source.
func TestPublicPoolIsSeparateAndTheDollarCapIsShared(t *testing.T) {
	t.Cleanup(Caps.Reset)
	db := spendDB(t)
	insertCall(t, db, "proxy", 600, 600, "datetime('now')")

	// Public traffic does not count against the unattended pool.
	if st := CheckSpend(db, 1000); st.Exceeded {
		t.Fatalf("proxy tokens leaked into the unattended pool: %+v", st)
	}
	// The default public budget is generous.
	if st := CheckPublicSpend(db); st.Exceeded || st.TokenBudget != DefaultPublicDailyTokenBudget {
		t.Fatalf("default public pool: %+v", st)
	}
	tight := 1000
	_ = SaveSpendLimits(db, SpendLimits{DailyPublicTokenBudget: &tight})
	if st := CheckPublicSpend(db); !st.Exceeded || st.TokenBudget != 1000 {
		t.Fatalf("project public budget not applied: %+v", st)
	}

	// The dollar cap counts engine and agent spend together, and binds both pools.
	Caps.DeclareCost(ModelKey{Provider: "p", Model: "priced"}, ModelCost{Input: 10, Output: 30})
	insertPriced(t, db, "engine", "p", "priced", 100_000, 0) // $1.00
	insertPriced(t, db, "agent", "p", "priced", 100_000, 0)  // $1.00
	capUSD := 1.5
	_ = SaveSpendLimits(db, SpendLimits{DailyUSDCap: &capUSD})
	if st := CheckSpend(db, 0); !st.Exceeded || st.USDUsed < 2 {
		t.Fatalf("unattended pool should see the shared dollar cap: %+v", st)
	}
	if st := CheckPublicSpend(db); !st.Exceeded {
		t.Fatalf("public pool should see the shared dollar cap: %+v", st)
	}
}
