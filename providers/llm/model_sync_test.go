package llm

import "testing"

// A number the listing reported is never overwritten by the name heuristic. What
// the endpoint says describes the deployment actually in front of us (a
// llama-server started with `-c 8192`), and a guess must not talk over it.
func TestFillFromName_ReportedNumbersWin(t *testing.T) {
	const smaller = 8192
	got := FillFromName(ModelInfo{ID: "gpt-5", ContextTokens: smaller, MaxOutputTokens: 1000})
	if got.ContextTokens != smaller || got.MaxOutputTokens != 1000 {
		t.Errorf("got %d/%d, want the reported %d/1000", got.ContextTokens, got.MaxOutputTokens, smaller)
	}
}

// A listing that reports only an id still gets a window and an output cap.
func TestFillFromName_FillsZeroes(t *testing.T) {
	got := FillFromName(ModelInfo{ID: "gpt-5"})
	if got.ContextTokens != 400000 || got.MaxOutputTokens != 128000 {
		t.Errorf("got %d/%d, want 400000/128000", got.ContextTokens, got.MaxOutputTokens)
	}
}

// An id no rule covers gets the conservative fallback window and an unknown
// (zero) output cap, never a made-up price.
func TestFillFromName_UnknownModel(t *testing.T) {
	got := FillFromName(ModelInfo{ID: "some-private-model"})
	if got.ContextTokens != fallbackContextWindow {
		t.Errorf("ContextTokens = %d, want the %d fallback", got.ContextTokens, fallbackContextWindow)
	}
	if got.MaxOutputTokens != 0 {
		t.Errorf("MaxOutputTokens = %d, want 0 (unknown)", got.MaxOutputTokens)
	}
	if got.Cost != (ModelCost{}) {
		t.Errorf("Cost = %+v, want zero: guessing a price would corrupt the budget tally", got.Cost)
	}
}

func TestNameLimits(t *testing.T) {
	cases := []struct {
		id       string
		ctx, out int
	}{
		{"gpt-6", 1050000, 128000},
		{"gpt-5.5", 1050000, 128000},
		{"gpt-5.4", 1050000, 128000},
		{"gpt-5.4-mini", 400000, 128000},
		{"gpt-5", 400000, 128000},
		{"gpt-5-mini", 400000, 128000},
		{"gpt-5-chat-latest", 128000, 16384},
		{"gpt-4.1", 1047576, 32768},
		{"gpt-4o", 128000, 16384},
		{"o3", 200000, 100000},
		{"o4-mini", 200000, 100000},
		{"chatgpt-4o-latest", 128000, 16384},
		{"glm-5.3", 1000000, 131072},
		{"glm-5.3-flash", 1000000, 131072},
		{"glm-5.1", 200000, 131072},
		{"glm-4.7", 200000, 131072},
		{"glm-4.5-air", 131072, 98304},
		{"claude-sonnet-4-5", 200000, 0},
		{"k3", 1048576, 131072},
		{"k3-256k", 262144, 131072},
		{"kimi-for-coding", 262144, 32768},
		{"MiniMax-M3", 1048576, 512000},
		{"MiniMax-M2.7-highspeed", 204800, 131072},
		{"qwen3.7-plus", 1000000, 65536},
		{"qwen3-coder-plus", 1000000, 65536},
		{"qwen3-coder-next", 262144, 65536},
		{"qwen3-max-2026-01-23", 262144, 32768},
	}
	for _, c := range cases {
		ctx, out := nameLimits(c.id)
		if ctx != c.ctx || out != c.out {
			t.Errorf("nameLimits(%q) = %d/%d, want %d/%d", c.id, ctx, out, c.ctx, c.out)
		}
	}
}

func TestOpenAISupportsVision(t *testing.T) {
	yes := []string{"gpt-6", "gpt-5.4", "gpt-4o", "o3", "glm-4.6v", "glm-5v-turbo"}
	no := []string{"gpt-5-nano", "glm-5", "glm-4.7", "some-private-model"}
	for _, id := range yes {
		if !openAISupportsVision(id) {
			t.Errorf("%s: want vision", id)
		}
	}
	for _, id := range no {
		if openAISupportsVision(id) {
			t.Errorf("%s: want no vision", id)
		}
	}
}

// The derived cache weights must reproduce the hand-tuned constants they
// replaced, because those constants were themselves an estimate OF this ratio.
// Anthropic publishes cache_read at 10% of input and cache_write at a 25%
// premium, which is exactly the 0.10 / 1.25 the old switch hardcoded.
func TestCacheCostFactorsFor_DerivesFromPublishedPrices(t *testing.T) {
	k := ModelKey{Provider: "Anthropic Claude", Model: "claude-sonnet-4-5"}
	t.Cleanup(Caps.Reset)

	// Unknown model: falls back to the per-transport estimate.
	if read, create := CacheCostFactorsFor(k, "anthropic"); read != 0.10 || create != 1.25 {
		t.Errorf("unknown model: got %v/%v, want the 0.10/1.25 transport fallback", read, create)
	}

	Caps.DeclareCost(k, ModelCost{Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75})
	read, create := CacheCostFactorsFor(k, "anthropic")
	if read <= 0 || read >= 1 {
		t.Errorf("cache read weight = %v, want a fraction of input price", read)
	}
	if create <= 1 {
		t.Errorf("cache write weight = %v, want a premium over input price", create)
	}
}

// A vendor with implicit caching publishes no cache_write price. That means
// creation bills as ordinary input - a weight of exactly 1 - not "unknown".
func TestCacheCostFactorsFor_ImplicitCacheWritesWeighOne(t *testing.T) {
	k := ModelKey{Provider: "Implicit Cache Co", Model: "m"}
	t.Cleanup(Caps.Reset)
	Caps.DeclareCost(k, ModelCost{Input: 1.0, Output: 2.0, CacheRead: 0.25})
	read, create := CacheCostFactorsFor(k, "openai-chat")
	if read != 0.25 {
		t.Errorf("read weight = %v, want 0.25 (cache_read / input)", read)
	}
	if create != 1.0 {
		t.Errorf("create weight = %v, want exactly 1: an absent cache_write price means creation bills as normal input", create)
	}
}
