// Rejection signatures this transport has learned to recognise, registered
// against the shared quirk machinery in providers/llm. The matchers live here,
// next to the request shaping they protect; the store, the retry loop, and the
// key scheme are shared.
package openai

import (
	"regexp"
	"strings"

	"openfabrica/providers/llm"
)

// forcedReasoningEffort is the level sent once thinking can no longer be turned
// off. "low" is valid on both the strict {low,high,max} set and the permissive
// one, so it needs no per-model mapping. Deliberately not req.Effort, which is
// "medium" pipeline-wide and rejected by the strict set.
const forcedReasoningEffort = "low"

func init() {
	llm.RegisterQuirkRule("openai-chat", llm.QuirkRule{
		Cap:   llm.CapMaxTokensField,
		Sent:  func(r llm.CompletionRequest) bool { return r.MaxTokens > 0 },
		Match: matchesMaxTokensField,
	})
	llm.RegisterQuirkRule("openai-chat", llm.QuirkRule{
		Cap:   llm.CapSamplingParams,
		Sent:  func(r llm.CompletionRequest) bool { return r.Temperature != nil || r.TopP != nil },
		Match: matchesSamplingParams,
	})
	llm.RegisterQuirkRule("openai-chat", llm.QuirkRule{
		Cap:   llm.CapParallelToolCalls,
		Sent:  func(r llm.CompletionRequest) bool { return len(r.Tools) > 0 },
		Match: matchesParallelToolCalls,
	})
	llm.RegisterQuirkRule("openai-chat", llm.QuirkRule{
		Cap:   llm.CapThinkingOff,
		Sent:  func(r llm.CompletionRequest) bool { return r.Thinking != nil && !*r.Thinking },
		Match: matchesForcedThinking,
	})
	llm.RegisterQuirkRule("openai-chat", llm.QuirkRule{
		Cap:   llm.CapThinkingField,
		Sent:  func(r llm.CompletionRequest) bool { return r.Thinking != nil },
		Match: matchesUnknownThinkingField,
	})
	llm.RegisterQuirkRule("openai-chat", llm.QuirkRule{
		Cap: llm.CapReasoningEffort,
		// Sent whenever the request would carry the field, which is any request
		// asking for thinking off. Independent of whether the thinking rule has
		// fired: an endpoint may take `thinking` and still reject the effort field.
		Sent:  func(r llm.CompletionRequest) bool { return r.Thinking != nil && !*r.Thinking },
		Match: matchesReasoningEffort,
	})
}

// matchesMaxTokensField recognises a reasoning model refusing max_tokens in favor
// of max_completion_tokens. Names the field, so an unrelated 400 never latches it.
func matchesMaxTokensField(e *llm.Error) bool {
	if e.Code != llm.CodeBadRequest {
		return false
	}
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "max_tokens") {
		return false
	}
	return strings.Contains(low, "max_completion_tokens") ||
		strings.Contains(low, "unsupported") ||
		strings.Contains(low, "not supported")
}

// matchesSamplingParams recognises a model refusing temperature or top_p (a
// reasoning model that only takes the default). Names the param and carries
// unsupported wording, so any other 400 keeps its fatal reading.
func matchesSamplingParams(e *llm.Error) bool {
	if e.Code != llm.CodeBadRequest {
		return false
	}
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "temperature") && !strings.Contains(low, "top_p") {
		return false
	}
	return strings.Contains(low, "unsupported") ||
		strings.Contains(low, "not supported") ||
		strings.Contains(low, "does not support") ||
		strings.Contains(low, "only the default") ||
		strings.Contains(low, "deprecated")
}

// matchesParallelToolCalls recognises a strict gateway refusing the field. Gated
// on CodeBadRequest so a 500 or transport failure echoing the request cannot
// latch it off.
func matchesParallelToolCalls(e *llm.Error) bool {
	return e.Code == llm.CodeBadRequest &&
		strings.Contains(strings.ToLower(e.Message), "parallel_tool_calls")
}

// matchesForcedThinking recognises an endpoint refusing to let thinking be
// turned off. Narrow on purpose: names thinking AND carries can't-disable
// wording. Not keyed on the vendor's numeric error code, which has no stability
// contract and could collide as a bare substring in some other body.
func matchesForcedThinking(e *llm.Error) bool {
	if e.Code != llm.CodeBadRequest {
		return false
	}
	low := strings.ToLower(e.Message)
	if !strings.Contains(low, "thinking") {
		return false
	}
	return strings.Contains(low, "cannot be disabled") ||
		strings.Contains(low, "can not be disabled") ||
		strings.Contains(low, "cannot disable") ||
		strings.Contains(low, "always engages in thinking") ||
		strings.Contains(low, "must be enabled")
}

// unknownThinkingParamRe anchors the FIELD NAME to the unknown-parameter
// wording (OpenAI "Unknown parameter: 'thinking'", vLLM "unexpected keyword
// argument", strict gateways' variants). Anchored on purpose: error messages
// carry the provider tag, and a provider merely NAMED with "thinking" must not
// convert an unrelated unknown-parameter complaint into a demotion.
var unknownThinkingParamRe = regexp.MustCompile(
	`(?i)(?:unknown parameter|unknown field|unrecognized request argument supplied|unsupported parameter|unexpected keyword(?: argument)?)[:\s]+['"]?(?:enable_)?thinking\b`)

// extraThinkingFieldRe recognises the pydantic shape, where the field name sits
// on its own line shortly before "extra fields not permitted". The line break
// reaches us as a LITERAL backslash-n (ClassifyHTTPError keeps the raw JSON
// body), so the lead-in accepts the escape as well as real whitespace.
var extraThinkingFieldRe = regexp.MustCompile(
	`(?i)(?:\\n|\s|")(?:enable_)?thinking\b[\s\S]{0,60}extra fields not permitted`)

// matchesUnknownThinkingField recognises a strict endpoint refusing the
// thinking toggle field itself in either direction. A can't-disable complaint
// is the forced-thinking rule's signature and is excluded so the two never race.
func matchesUnknownThinkingField(e *llm.Error) bool {
	if e.Code != llm.CodeBadRequest || matchesForcedThinking(e) {
		return false
	}
	return unknownThinkingParamRe.MatchString(e.Message) || extraThinkingFieldRe.MatchString(e.Message)
}

// matchesReasoningEffort recognises an endpoint refusing the reasoning_effort
// field itself, as opposed to the level it was given.
func matchesReasoningEffort(e *llm.Error) bool {
	return e.Code == llm.CodeBadRequest &&
		strings.Contains(strings.ToLower(e.Message), "reasoning_effort")
}
