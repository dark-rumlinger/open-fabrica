package llm

// CacheCostFactors approximates how cached prefix tokens are priced relative to
// a fresh input token. Used to render an "effective input" figure so cache-heavy
// runs are not misread as expensive (or vice versa) by a budget counted in
// tokens rather than dollars.
//
// This is the FALLBACK, for a model with no price on record. Prefer
// CacheCostFactorsFor, which derives the same two numbers from the model's real
// prices.
//
//   - anthropic: explicit prompt caching - reads ~10% of input price, writes
//     carry a ~25% premium.
//   - openai-chat and friends (OpenAI, Z.ai GLM, DeepSeek, Gemini-compat):
//     implicit prefix caching - reads commonly land at ~20-50% of input price
//     (GLM ~1/5), with no separate write premium (creation bills as normal
//     input, already counted in input_tokens).
func CacheCostFactors(providerType string) (read, create float64) {
	switch providerType {
	case "anthropic":
		return 0.10, 1.25
	default:
		return 0.20, 1.00
	}
}

// CacheCostFactorsFor returns the cache weights for one model, derived from the
// prices recorded for it: a cached read costs cache_read/input of a
// fresh token, a cache write costs cache_write/input.
//
// This is why the estimates above could only ever be estimates: keyed on the
// TRANSPORT, which dozens of vendors share with completely different cache
// pricing, every provider on openai-chat got the same 0.20 whether its cached
// reads were free or half price. Keyed on the model, the number is the vendor's
// own ratio.
//
// Falls back to the transport estimate when no price is on record, the honest
// answer for a private endpoint.
func CacheCostFactorsFor(k ModelKey, providerType string) (read, create float64) {
	cost := Caps.Cost(k)
	if cost.Input <= 0 {
		return CacheCostFactors(providerType)
	}
	read, _ = CacheCostFactors(providerType)
	if cost.CacheRead > 0 {
		read = cost.CacheRead / cost.Input
	}
	// A zero cache_write means the vendor does implicit caching and bills
	// creation as ordinary input, already counted in input_tokens - so the
	// weight is exactly 1, not "unknown".
	if cost.CacheWrite > 0 {
		create = cost.CacheWrite / cost.Input
	} else {
		create = 1.0
	}
	return read, create
}

// CallCostUSD prices one call from the model's recorded prices. Token counts follow the
// log columns: input is the non-cached portion only. ok is false when no
// price is on record for the model (a private endpoint, say), in which case
// showing $0 would be wrong.
func CallCostUSD(provider, model string, input, output, cacheRead, cacheWrite int) (usd float64, ok bool) {
	c := Caps.Cost(ModelKey{Provider: provider, Model: model})
	if c.Input <= 0 && c.Output <= 0 {
		return 0, false
	}
	total := float64(input)*c.Input + float64(output)*c.Output +
		float64(cacheRead)*c.CacheRead + float64(cacheWrite)*c.CacheWrite
	return total / 1e6, true
}
