package llm

import (
	"errors"
	"strings"
	"testing"
)

// registerTestRule installs a rule under a throwaway transport and removes it
// afterwards, so the package-level registry does not leak between tests.
func registerTestRule(t *testing.T, transport string, rules ...QuirkRule) {
	t.Helper()
	for _, r := range rules {
		RegisterQuirkRule(transport, r)
	}
	t.Cleanup(func() {
		quirkMu.Lock()
		delete(quirkRules, transport)
		quirkMu.Unlock()
		Caps.Reset()
	})
}

func badRequest(msg string) error {
	return &Error{Code: CodeBadRequest, Status: 400, Message: msg}
}

func namesRule(cap, needle string) QuirkRule {
	return QuirkRule{
		Cap:   cap,
		Sent:  func(CompletionRequest) bool { return true },
		Match: func(e *Error) bool { return strings.Contains(e.Message, needle) },
	}
}

func TestLearnFromError_DemotesTheMatchedCapability(t *testing.T) {
	registerTestRule(t, "tt-basic", namesRule(CapEffort, "effort"))
	k := ModelKey{Provider: "p", Model: "m"}

	if !LearnFromError("tt-basic", k, CompletionRequest{}, badRequest("unsupported: effort")) {
		t.Fatal("a matching rejection taught nothing")
	}
	if Caps.Allowed(k, CapEffort) {
		t.Error("capability still allowed after its rule fired")
	}
}

// Untyped errors must never reach a matcher. A transport failure or a 500 whose
// text echoes the request would otherwise latch a field off permanently.
func TestLearnFromError_IgnoresUntypedAndNonBadRequest(t *testing.T) {
	registerTestRule(t, "tt-typed", QuirkRule{
		Cap:   CapEffort,
		Sent:  func(CompletionRequest) bool { return true },
		Match: func(e *Error) bool { return e.Code == CodeBadRequest && strings.Contains(e.Message, "effort") },
	})
	k := ModelKey{Provider: "p", Model: "m"}

	if LearnFromError("tt-typed", k, CompletionRequest{}, errors.New("effort")) {
		t.Error("an untyped error reached a matcher")
	}
	if LearnFromError("tt-typed", k, CompletionRequest{}, &Error{Code: CodeServer, Message: "effort"}) {
		t.Error("a 500 naming the field was treated as a rejection")
	}
}

// The Sent guard is what stops a 400 being blamed on a field the request never
// carried.
func TestLearnFromError_SkipsFieldsNotSent(t *testing.T) {
	registerTestRule(t, "tt-sent", QuirkRule{
		Cap:   CapTools,
		Sent:  func(r CompletionRequest) bool { return len(r.Tools) > 0 },
		Match: func(*Error) bool { return true },
	})
	k := ModelKey{Provider: "p", Model: "m"}

	if LearnFromError("tt-sent", k, CompletionRequest{}, badRequest("anything")) {
		t.Error("learned about a field the request did not send")
	}
}

// The retry loop is bounded by the rule count, not a hand-picked number, and a
// repeat Learn reporting false is what makes that hold.
func TestRetryOnQuirk_StopsWhenNothingNewIsLearned(t *testing.T) {
	registerTestRule(t, "tt-loop", namesRule(CapEffort, "effort"))
	k := ModelKey{Provider: "p", Model: "m"}

	calls := 0
	_, err := RetryOnQuirk("tt-loop", k, CompletionRequest{}, func() (int, error) {
		calls++
		return 0, badRequest("unsupported: effort")
	})
	if err == nil {
		t.Fatal("want the failure surfaced once the loop gives up")
	}
	// One original attempt plus one retry after the single rule fires.
	if calls != 2 {
		t.Errorf("%d attempts, want 2 (original + one retry for one rule)", calls)
	}
}

func TestRetryOnQuirk_WalksEveryRuleThenStops(t *testing.T) {
	registerTestRule(t, "tt-two",
		namesRule(CapEffort, "rejected"),
		namesRule(CapSamplingParams, "rejected"),
	)
	k := ModelKey{Provider: "p", Model: "m"}

	calls := 0
	_, _ = RetryOnQuirk("tt-two", k, CompletionRequest{}, func() (int, error) {
		calls++
		return 0, badRequest("rejected")
	})
	if want := 1 + QuirkRuleCount("tt-two"); calls != want {
		t.Errorf("%d attempts, want %d (1 + rule count)", calls, want)
	}
}

func TestRetryOnQuirk_SuccessSkipsTheLoop(t *testing.T) {
	registerTestRule(t, "tt-ok", namesRule(CapEffort, "effort"))
	calls := 0
	got, err := RetryOnQuirk("tt-ok", ModelKey{Provider: "p", Model: "m"}, CompletionRequest{}, func() (int, error) {
		calls++
		return 7, nil
	})
	if err != nil || got != 7 || calls != 1 {
		t.Errorf("got %d, err %v, calls %d; want 7, nil, 1", got, err, calls)
	}
}

func TestValidateModelCaps_RejectsUnknownCapability(t *testing.T) {
	registerTestRule(t, "tt-bad", QuirkRule{
		Cap:   "not-a-real-capability",
		Match: func(*Error) bool { return false },
	})
	errs := ValidateModelCaps()
	if len(errs) == 0 {
		t.Fatal("a rule naming an unknown capability passed validation")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "not-a-real-capability") {
			found = true
		}
	}
	if !found {
		t.Errorf("no error names the offending capability: %v", errs)
	}
}
