package engine

// Tests for the retry layer's aggregate backoff ceiling and the call accounting the
// LLM log keys on. Both exist because duration alone cannot tell a slow provider
// from a throttled one.

import (
	"context"
	"testing"
	"time"

	"openfabrica/providers/llm"
)

// countingProvider fails with the given code until it has been called okAfter times.
type countingProvider struct {
	calls   int
	okAfter int
	code    string
}

func (c *countingProvider) Name() string { return "counting" }
func (c *countingProvider) Type() string { return "counting" }
func (c *countingProvider) Complete(ctx context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	c.calls++
	if c.calls >= c.okAfter {
		return &llm.CompletionResponse{Content: "ok"}, nil
	}
	return nil, &llm.Error{Code: c.code, Message: "throttled"}
}
func (c *countingProvider) Stream(context.Context, llm.CompletionRequest, llm.StreamCallback) error {
	return nil
}
func (c *countingProvider) ListModels(context.Context) ([]llm.ModelInfo, error) { return nil, nil }
func (c *countingProvider) Ping(context.Context, string) error                  { return nil }

// fixedBackoff is a policy whose every wait is d, so a test can reason in whole units.
func fixedBackoff(d time.Duration) func(int) time.Duration {
	return func(int) time.Duration { return d }
}

// The aggregate ceiling stops a call that would otherwise keep sleeping through its
// whole per-class attempt budget. Without it, rate-limit alone is 6 attempts at up to
// 180s, and the fallback provider then re-runs the same budget.
func TestRetryStopsAtTotalWaitCeiling(t *testing.T) {
	p := &countingProvider{okAfter: 99, code: llm.CodeRateLimit}
	sp := StagePolicy{
		RateLimit:    Policy{MaxAttempts: 6, Backoff: fixedBackoff(20 * time.Millisecond)},
		MaxTotalWait: 50 * time.Millisecond,
	}
	_, stats, err := sp.CallLLM(context.Background(), nil, p, llm.CompletionRequest{}, 0, nil)
	if err == nil {
		t.Fatal("a provider that never succeeds must return an error")
	}
	// Two sleeps of 20ms fit under 50ms; the third would exceed it and gives up.
	if stats.Waited > sp.MaxTotalWait {
		t.Errorf("waited %v, over the %v ceiling", stats.Waited, sp.MaxTotalWait)
	}
	if p.calls >= 6 {
		t.Errorf("ceiling did not cut the attempt budget short: %d provider calls", p.calls)
	}
}

// A zero ceiling leaves the per-class budgets in charge, so an existing deployment
// that never sets one behaves exactly as before.
func TestRetryWithoutCeilingUsesAttemptBudget(t *testing.T) {
	p := &countingProvider{okAfter: 99, code: llm.CodeRateLimit}
	sp := StagePolicy{RateLimit: Policy{MaxAttempts: 3, Backoff: fixedBackoff(time.Millisecond)}}
	_, stats, err := sp.CallLLM(context.Background(), nil, p, llm.CompletionRequest{}, 0, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if p.calls != 3 || stats.Attempts != 3 {
		t.Errorf("want 3 attempts, got provider calls=%d stats=%d", p.calls, stats.Attempts)
	}
}

// A call that succeeds first time reports one attempt and no waiting, so a log row
// with a large duration and zero retry_wait_ms points at the provider, not at us.
func TestRetryStatsOnCleanCall(t *testing.T) {
	p := &countingProvider{okAfter: 1}
	sp := defaultStagePolicy
	_, stats, err := sp.CallLLM(context.Background(), nil, p, llm.CompletionRequest{}, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Attempts != 1 || stats.Waited != 0 || stats.UsedFallback {
		t.Errorf("clean call reported %+v", stats)
	}
}

// A retried call that eventually succeeds separates the sleeping from the provider
// time, which is the whole point of recording both.
func TestRetryStatsCountWaiting(t *testing.T) {
	p := &countingProvider{okAfter: 3, code: llm.CodeRateLimit}
	sp := StagePolicy{RateLimit: Policy{MaxAttempts: 6, Backoff: fixedBackoff(10 * time.Millisecond)}}
	_, stats, err := sp.CallLLM(context.Background(), nil, p, llm.CompletionRequest{}, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Attempts != 3 {
		t.Errorf("want 3 attempts, got %d", stats.Attempts)
	}
	if stats.Waited < 20*time.Millisecond {
		t.Errorf("want at least two recorded backoffs, got %v", stats.Waited)
	}
}
