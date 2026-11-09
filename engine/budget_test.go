package engine

import (
	"context"
	"testing"

	"openfabrica/engine/enginetest"
)

// TestEngineTokensUsedToday counts only today's engine calls, excluding chat,
// and cost-weights cached tokens. The row has no provider_type, so the default
// factors apply (read 0.20, create 1.00).
func TestEngineTokensUsedToday(t *testing.T) {
	_, pdb := testCaller(t)
	w := pdb.Writer()
	w.Exec(`INSERT INTO of_llm_calls (source, model, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens) VALUES ('engine','m',100,200,10,5)`)
	w.Exec(`INSERT INTO of_llm_calls (source, model, input_tokens, output_tokens) VALUES ('chat','m',999,999)`)                                     // excluded
	w.Exec(`INSERT INTO of_llm_calls (source, model, input_tokens, output_tokens, created_at) VALUES ('engine','m',500,500,'2000-01-01 00:00:00')`) // excluded (old)

	got, err := engineTokensUsedToday(pdb)
	if err != nil {
		t.Fatalf("engineTokensUsedToday: %v", err)
	}
	// 100 input + 200 output + 10*0.20 read + 5*1.00 create = 307.
	if got != 307 {
		t.Errorf("today engine tokens = %d; want 307 (chat + old excluded, cache cost-weighted)", got)
	}
}

// TestEngineTokensUsedTodayCostWeighted: cached tokens are weighted per
// provider type, so cache-heavy reuse doesn't burn the budget at its raw token
// count. Anthropic (read 0.10, create 1.25) and the default bucket differ.
func TestEngineTokensUsedTodayCostWeighted(t *testing.T) {
	_, pdb := testCaller(t)
	w := pdb.Writer()
	// Anthropic: 1000 + 0 + 100000*0.10 + 10000*1.25 = 23500.
	w.Exec(`INSERT INTO of_llm_calls (source, model, provider_type, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens) VALUES ('engine','m','anthropic',1000,0,100000,10000)`)
	// Default bucket: 1000 + 0 + 100000*0.20 + 10000*1.00 = 31000.
	w.Exec(`INSERT INTO of_llm_calls (source, model, provider_type, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens) VALUES ('engine','m','openai-chat',1000,0,100000,10000)`)

	got, err := engineTokensUsedToday(pdb)
	if err != nil {
		t.Fatalf("engineTokensUsedToday: %v", err)
	}
	if got != 54500 { // 23500 + 31000
		t.Errorf("cost-weighted engine tokens = %d; want 54500", got)
	}
}

// TestDailyTokenBudgetPausesAtCeiling: once a project crosses its daily budget,
// the next billable call is refused with a permanent, owner-pausable error -
// while a zero budget never gates (covered by every other caller test).
func TestDailyTokenBudgetPausesAtCeiling(t *testing.T) {
	c, pdb := testCaller(t)
	c.budgetFn = func() int { return 1000 }

	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("first"), enginetest.Text("second"),
	}}

	// Under budget: the call goes through.
	if _, _, _, err := c.call(context.Background(), callerParams(p)); err != nil {
		t.Fatalf("under-budget call failed: %v", err)
	}

	// Record prior engine spend that crosses the ceiling for today.
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_llm_calls (source, model, input_tokens, output_tokens) VALUES ('engine','m',600,600)`,
	); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	// The next billable call (distinct content so it can't replay from cache) is
	// refused with a budget error.
	params := callerParams(p)
	params.req.Messages[0].Content = "second distinct request"
	_, _, _, err := c.call(context.Background(), params)
	if err == nil {
		t.Fatal("expected budget error once over the daily ceiling, got nil")
	}
	if !isBudgetError(err) {
		t.Fatalf("error not classified as budget: %v", err)
	}
	if !isPermanentError(err) {
		t.Fatal("budget error must be permanent so the worker pauses instead of retrying")
	}
	if msg, ok := budgetPauseMessage(err); !ok || msg == "" {
		t.Fatal("budgetPauseMessage should produce an owner-facing message")
	}
}
