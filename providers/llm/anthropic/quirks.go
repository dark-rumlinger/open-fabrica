// Rejection signatures this transport has learned to recognise, registered
// against the shared quirk machinery in providers/llm.
//
// There is no baked-in list of which models dropped which field: the Models API
// does not report it and hardcoded names go stale. Fields are sent
// optimistically and the first 400 teaches the store. Cost is one wasted
// round-trip per field per model, and zero names to keep current.
package anthropic

import (
	"strings"

	"openfabrica/providers/llm"
)

func init() {
	llm.RegisterQuirkRule("anthropic", llm.QuirkRule{
		Cap:   llm.CapSamplingParams,
		Sent:  func(r llm.CompletionRequest) bool { return r.Temperature != nil || r.TopP != nil },
		Match: matchesSamplingParamRejection,
	})
	llm.RegisterQuirkRule("anthropic", llm.QuirkRule{
		Cap:   llm.CapEffort,
		Sent:  func(r llm.CompletionRequest) bool { return r.Effort != "" },
		Match: matchesEffortRejection,
	})
	llm.RegisterQuirkRule("anthropic", llm.QuirkRule{
		Cap:   llm.CapForcedToolChoice,
		Sent:  func(r llm.CompletionRequest) bool { return r.ResponseSchema != nil },
		Match: matchesForcedToolChoiceRejection,
	})
}

// matchesForcedToolChoiceRejection recognises the newer models refusing a forced
// tool_choice (Sonnet 5.5, Opus 5.5, Fable 5.1): 400 'tool_choice: type "tool" and
// "any" are not supported for this model.' The structured-output path then keeps the
// single response tool but selects it with tool_choice=auto.
func matchesForcedToolChoiceRejection(e *llm.Error) bool {
	if e.Code != llm.CodeBadRequest {
		return false
	}
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "tool_choice") {
		return false
	}
	return strings.Contains(low, "not supported") ||
		strings.Contains(low, "unsupported") ||
		strings.Contains(low, "not accept") ||
		strings.Contains(low, "does not support")
}

// matchesSamplingParamRejection recognises the API refusing a sampling parameter
// a newer model no longer accepts. Narrow: names one of the three params
// together with deprecation wording, so any other 400 keeps its fatal reading.
func matchesSamplingParamRejection(e *llm.Error) bool {
	if e.Code != llm.CodeBadRequest {
		return false
	}
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "temperature") && !strings.Contains(low, "top_p") && !strings.Contains(low, "top_k") {
		return false
	}
	return strings.Contains(low, "deprecated") ||
		strings.Contains(low, "not supported") ||
		strings.Contains(low, "unsupported") ||
		strings.Contains(low, "not accept")
}

// matchesEffortRejection recognises an older model refusing output_config.effort.
// The value the engine sends is valid on every model that has the control, so a
// rejection always means "no effort control", never "bad level".
func matchesEffortRejection(e *llm.Error) bool {
	if e.Code != llm.CodeBadRequest {
		return false
	}
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "effort") && !strings.Contains(low, "output_config") {
		return false
	}
	return strings.Contains(low, "not supported") ||
		strings.Contains(low, "unsupported") ||
		strings.Contains(low, "deprecated") ||
		strings.Contains(low, "not accept") ||
		strings.Contains(low, "does not support")
}
