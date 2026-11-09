package llm

import (
	"context"
	"strings"
)

// Capability probes: one small live call that settles a capability the model listing
// does not report. Each returns a verdict and the call's error, so a caller probing
// many models can stop when the provider starts rate limiting.

// ProbeVerdict is what one probe learned.
type ProbeVerdict int

const (
	// ProbeUnknown: the probe failed (auth, rate limit, timeout); ask again later.
	ProbeUnknown ProbeVerdict = iota
	// ProbeYes: the model has the capability.
	ProbeYes
	// ProbeNo: the model does not.
	ProbeNo
)

// IsRateLimited reports whether err says the provider is throttling or out of quota,
// the point at which a batch of probes should stop.
func IsRateLimited(err error) bool {
	e, ok := AsLLMError(err)
	return ok && (e.Code == CodeRateLimit || e.Code == CodeQuotaExceeded || e.Code == CodeOverloaded)
}

// reasoningProbePrompt needs a moment of thought, so a model with a thinking mode
// takes it, and is short, so a model without one answers at once.
const reasoningProbePrompt = "What is 17 + 25? Reply with the number only."

// ProbeReasoning asks the model one small question with thinking on and reads the
// first token it streams: a reasoning token means it has a thinking mode, an answer
// token means it does not. The stream is cut at that first token, so the probe costs
// a few input tokens and the first output token. A model that thinks inline (Ollama
// and vLLM models that write <think> into the content) counts as reasoning.
func ProbeReasoning(ctx context.Context, p Provider, model string) (ProbeVerdict, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	on := true
	verdict := ProbeUnknown
	var content strings.Builder
	err := p.Stream(ctx, CompletionRequest{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Content: reasoningProbePrompt}},
		MaxTokens: 256,
		Thinking:  &on,
	}, func(c StreamChunk) {
		if verdict != ProbeUnknown {
			return
		}
		if c.Reasoning != "" {
			verdict = ProbeYes
			cancel()
			return
		}
		content.WriteString(c.Delta)
		// Wait for enough content to tell an inline <think> block from an answer.
		if text := strings.TrimSpace(content.String()); len(text) >= len("<think>") || (text != "" && !strings.HasPrefix("<think>", text)) {
			if strings.HasPrefix(text, "<think>") {
				verdict = ProbeYes
			} else {
				verdict = ProbeNo
			}
			cancel()
		}
	})
	if verdict == ProbeUnknown && strings.TrimSpace(content.String()) != "" {
		// The stream ended on a short answer before the length check settled it.
		verdict = ProbeNo
	}
	if verdict != ProbeUnknown {
		return verdict, nil
	}
	return ProbeUnknown, err
}

// toolsProbeTool is the one tool the tools probe offers.
var toolsProbeTool = ToolDef{
	Name:        "add_numbers",
	Description: "Adds two integers and returns the sum.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "integer"},
			"b": map[string]any{"type": "integer"},
		},
		"required": []string{"a", "b"},
	},
}

// ProbeTools asks the model to call a tool. A tool call means it supports tool use;
// a prose answer means it does not. An endpoint that refuses the tools field (400)
// does not support them either. Thinking is off so a reasoning model answers at once;
// tool calls arrive whole, so this is one short non-streamed call.
func ProbeTools(ctx context.Context, p Provider, model string) (ProbeVerdict, error) {
	off := false
	resp, err := p.Complete(ctx, CompletionRequest{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Content: "Use the add_numbers tool to add 17 and 25."}},
		Tools:     []ToolDef{toolsProbeTool},
		MaxTokens: 200,
		Thinking:  &off,
	})
	if err != nil {
		if e, ok := AsLLMError(err); ok && e.Code == CodeBadRequest {
			return ProbeNo, nil
		}
		return ProbeUnknown, err
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name == toolsProbeTool.Name {
			return ProbeYes, nil
		}
	}
	return ProbeNo, nil
}
