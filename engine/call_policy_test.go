package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
)

// Live chat and trigger/job LLM calls ride retries through Engine.CallLLM under the
// LIVE retry policy, so a single 429 does not fail the owner's message.
func TestCallLLMRetriesRateLimit(t *testing.T) {
	e := New(&Deps{}, context.Background())
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Fail(&llm.Error{Code: llm.CodeRateLimit, Message: "slow down", RetryAfter: time.Millisecond}),
		enginetest.Text("hello"),
	}}
	var call llm.CallFunc = e.CallLLM
	resp, err := call.Call(context.Background(), 0, p, llm.CompletionRequest{Model: "m"})
	if err != nil {
		t.Fatalf("CallLLM: %v", err)
	}
	if resp.Content != "hello" || p.CallCount() != 2 {
		t.Fatalf("content=%q calls=%d; want the retry to answer on the second call", resp.Content, p.CallCount())
	}
}

// A known provider id holds a slot in the engine's shared per-model limiter:
// with every slot taken, the call waits (here: until its context gives up).
func TestCallLLMSharesTheModelLimiter(t *testing.T) {
	e := New(&Deps{MaxConcurrentPerModel: 1}, context.Background())
	release, err := e.limiter.acquire(context.Background(), 7, "m")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p := &enginetest.Provider{}
	_, err = e.CallLLM(ctx, 7, p, llm.CompletionRequest{Model: "m"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want the call to wait on the full bucket", err)
	}
	if p.CallCount() != 0 {
		t.Fatal("the provider was called without a limiter slot")
	}

	// A nil CallFunc calls the provider directly.
	var direct llm.CallFunc
	if _, err := direct.Call(context.Background(), 7, p, llm.CompletionRequest{Model: "m"}); err != nil || p.CallCount() != 1 {
		t.Fatalf("nil CallFunc: err=%v calls=%d", err, p.CallCount())
	}
}
