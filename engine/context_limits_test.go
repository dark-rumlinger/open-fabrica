package engine

import (
	"context"
	"testing"

	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
)

func overflowErr() error {
	return &llm.Error{Code: llm.CodeContextOverflow, Status: 400, Message: "context_length_exceeded: prompt is too long"}
}

// A provider that rejects the prompt as too large after calls that fit is telling the
// engine its window is smaller than the default trigger. The segment must roll over
// (reseed smaller) instead of failing the run, and later segments must roll over below
// the size that was rejected rather than hit the wall again.
func TestSessionContextOverflowRollsOverAndLearnsCeiling(t *testing.T) {
	small := func(st enginetest.Step) enginetest.Step {
		st.Resp.Usage = llm.Usage{InputTokens: 2, OutputTokens: 1}
		return st
	}
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(echoCall("c1", "fits")), // accepted: 10 input tokens
		enginetest.Fail(overflowErr()),           // the next, larger prompt does not fit
		small(enginetest.Calls(echoCall("c2", "after roll"))),
		small(enginetest.Text("done")),
	}}
	s := testSession(t, p)
	spec := buildSpecForTest()
	reseeds := 0
	spec.Reseed = func() ([]llm.Message, error) {
		reseeds++
		return []llm.Message{{Role: llm.RoleUser, Content: "recap: continue"}}, nil
	}

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("an overflow after a fitting call must roll over, not fail: %v", err)
	}
	if reseeds != 1 || res.Segments != 2 || res.Content != "done" {
		t.Fatalf("reseeds=%d segments=%d content=%q", reseeds, res.Segments, res.Content)
	}
	if want := 8; s.rolloverCeiling != want {
		t.Fatalf("learned ceiling = %d, want %d (80%% of the last accepted prompt)", s.rolloverCeiling, want)
	}
}

// A segment whose very first call overflows has nothing smaller to reseed from: the
// failure must surface instead of looping through rollovers.
func TestSessionContextOverflowOnFirstCallFails(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Fail(overflowErr())}}
	s := testSession(t, p)
	spec := buildSpecForTest()
	spec.Reseed = func() ([]llm.Message, error) {
		return []llm.Message{{Role: llm.RoleUser, Content: "recap"}}, nil
	}
	if _, err := s.Run(context.Background(), spec); err == nil {
		t.Fatal("an overflow with no accepted call in the segment must be an error")
	}
	if s.rolloverCeiling != 0 {
		t.Fatalf("no ceiling can be learned without an accepted call, got %d", s.rolloverCeiling)
	}
}

// A model that caps its reply below the engine's max_tokens rejects the request. The
// engine learns the real cap from the message and retries once at it, then keeps
// sending the lower figure.
func TestSessionLearnsOutputCapFromRejection(t *testing.T) {
	rej := &llm.Error{Code: llm.CodeBadRequest, Status: 400,
		Message: "max_tokens: 32768 > 8192, which is the maximum allowed number of output tokens for this model"}
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Fail(rej), enginetest.Text("ok")}}
	s := testSession(t, p)
	t.Cleanup(func() { outputCaps.Delete(outputCapKey(llm.ModelKey{Provider: "enginetest", Model: "fake-model"})) })
	spec := buildSpecForTest()
	spec.PreloadTools = nil
	spec.MaxTokens = 32768

	res, err := s.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("a max_tokens rejection must be retried at the learned cap: %v", err)
	}
	if res.Content != "ok" || len(p.Requests) != 2 {
		t.Fatalf("content=%q requests=%d", res.Content, len(p.Requests))
	}
	if got := p.Requests[1].MaxTokens; got != 8192 {
		t.Fatalf("retry max_tokens = %d, want the learned 8192", got)
	}
	if got := clampMaxTokens(llm.ModelKey{Provider: "enginetest", Model: "fake-model"}, 32768); got != 8192 {
		t.Fatalf("later requests send %d, want 8192", got)
	}
}

func TestLearnOutputCapReadsVendorWordings(t *testing.T) {
	for _, tc := range []struct {
		name, msg string
		want      int
	}{
		{"anthropic", "max_tokens: 32768 > 8192, which is the maximum allowed number of output tokens for claude-3-5-haiku", 8192},
		{"openai", "max_tokens is too large: 32768. This model supports at most 16384 completion tokens, whereas you provided 32768.", 16384},
		{"deepseek", "Invalid max_tokens value, the valid range of max_tokens is [1, 8192]", 8192},
		{"unrelated bad request", "unknown parameter: top_k", 0},
		{"no number below the request", "max_tokens must be set lower", 0},
	} {
		err := &llm.Error{Code: llm.CodeBadRequest, Status: 400, Message: tc.msg}
		if got := learnOutputCap(err, 32768); got != tc.want {
			t.Errorf("%s: learnOutputCap = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := learnOutputCap(&llm.Error{Code: llm.CodeServer, Message: "max_tokens 8192"}, 32768); got != 0 {
		t.Errorf("a non-bad-request error must not teach a cap, got %d", got)
	}
}
