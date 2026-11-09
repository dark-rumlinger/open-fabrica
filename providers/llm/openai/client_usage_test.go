package openai

import (
	"testing"

	"openfabrica/providers/llm"
)

// The usage normalization assumes OpenAI's convention: prompt_tokens is the
// TOTAL input and INCLUDES the cached prefix, so the cached portion is
// subtracted out to avoid double-counting it against CacheReadInputTokens.
//
// This client is the shared engine for every OpenAI-compatible base_url, and that convention is not
// universal - Anthropic's is the opposite, reporting the uncached remainder. An
// endpoint reporting cached_tokens > prompt_tokens has proven it does not follow
// the inclusive convention, because a subset cannot exceed its superset.
//
// InputTokens must never go negative: it is SUM(input_tokens) in the daily
// budget tally (providers/llm/budget.go), where a negative row drives the
// running total AWAY from the ceiling. Enough of them and the spend limit -
// which ships on by default at 1M tokens/project/day - can never trip.
func TestParseResponseNeverReportsNegativeInputTokens(t *testing.T) {
	c := &Client{}
	// An endpoint following the exclusive convention: 200 fresh input tokens on
	// top of an 800-token cached prefix.
	resp := &apiResponse{
		Model: "some-compatible-model",
		Usage: apiUsage{
			PromptTokens:        200,
			CompletionTokens:    50,
			PromptTokensDetails: &promptTokensDetails{CachedTokens: 800},
		},
	}

	got := c.parseResponse(resp)

	if got.Usage.InputTokens < 0 {
		t.Errorf("InputTokens = %d, must never be negative (it feeds SUM(input_tokens) in the daily budget tally)",
			got.Usage.InputTokens)
	}
	// Having established the endpoint is not inclusive, prompt_tokens IS the
	// uncached input and should be taken at face value rather than subtracted from.
	if got.Usage.InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200 (prompt_tokens taken as-is once the inclusive reading is disproven)",
			got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 800 {
		t.Errorf("CacheReadInputTokens = %d, want 800", got.Usage.CacheReadInputTokens)
	}
}

// The ordinary OpenAI case must keep working: prompt_tokens includes the cached
// prefix, so it is subtracted out and the two fields don't double-count.
func TestParseResponseSubtractsCachedFromInclusivePromptTokens(t *testing.T) {
	c := &Client{}
	resp := &apiResponse{
		Model: "gpt-4o",
		Usage: apiUsage{
			PromptTokens:        1000, // total, cached portion included
			CompletionTokens:    50,
			PromptTokensDetails: &promptTokensDetails{CachedTokens: 800},
		},
	}

	got := c.parseResponse(resp)

	if got.Usage.InputTokens != 200 {
		t.Errorf("InputTokens = %d, want 200 (1000 total - 800 cached)", got.Usage.InputTokens)
	}
	if got.Usage.CacheReadInputTokens != 800 {
		t.Errorf("CacheReadInputTokens = %d, want 800", got.Usage.CacheReadInputTokens)
	}
}

// normalizeUsage is shared by the buffered and streaming paths (the streaming
// one is what the build loop actually runs), so its boundaries are pinned here
// rather than through two transport harnesses.
func TestNormalizeUsageBoundaries(t *testing.T) {
	cases := []struct {
		name                       string
		prompt, completion, cached int
		wantInput, wantCacheRead   int
	}{
		{"no cache reported", 500, 20, 0, 500, 0},
		{"inclusive: cached is a subset", 1000, 20, 800, 200, 800},
		{"inclusive: fully cached prefix", 800, 20, 800, 0, 800},
		{"exclusive: cached exceeds prompt", 200, 20, 800, 200, 800},
		{"empty usage", 0, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeUsage(tc.prompt, tc.completion, tc.cached)
			if got.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d", got.InputTokens, tc.wantInput)
			}
			if got.CacheReadInputTokens != tc.wantCacheRead {
				t.Errorf("CacheReadInputTokens = %d, want %d", got.CacheReadInputTokens, tc.wantCacheRead)
			}
			if got.OutputTokens != tc.completion {
				t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, tc.completion)
			}
			if got.InputTokens < 0 {
				t.Errorf("InputTokens = %d, must never be negative", got.InputTokens)
			}
		})
	}
}

// A negative input count doesn't just under-report - it actively subtracts from
// the running tally, so unattended spend moves backwards and the ceiling recedes
// as the project burns tokens. This pins the consequence, not just the field.
func TestNegativeInputTokensCannotDriveBudgetTallyBackwards(t *testing.T) {
	c := &Client{}
	resp := &apiResponse{
		Model: "some-compatible-model",
		Usage: apiUsage{
			PromptTokens:        200,
			CompletionTokens:    10,
			PromptTokensDetails: &promptTokensDetails{CachedTokens: 800},
		},
	}
	u := c.parseResponse(resp).Usage

	// Mirror the weighting TokensUsedTodayForSources applies per row.
	readF, createF := llm.CacheCostFactors("openai-chat")
	weighted := int64(u.InputTokens) + int64(u.OutputTokens) +
		int64(float64(u.CacheReadInputTokens)*readF) +
		int64(float64(u.CacheCreationInputTokens)*createF)

	if weighted <= 0 {
		t.Fatalf("a real billed call contributed %d to the daily budget tally: spend counts backwards and the ceiling never trips", weighted)
	}
}
