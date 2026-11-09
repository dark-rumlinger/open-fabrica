// Package enginetest provides a scripted fake llm.Provider so engine and
// stage behaviour can be tested offline - no API key, no network. Modelled on
// the stubProvider in llm/replay_provider_test.go, with a call recorder and
// scripted error support added.
package enginetest

import (
	"context"
	"sync"

	"openfabrica/providers/llm"
)

// Step is one scripted turn: either a response or an error.
type Step struct {
	Resp *llm.CompletionResponse
	Err  error
}

// Provider replays scripted steps in order. When the script runs out it
// returns Default (or a plain "done" response when Default is nil).
type Provider struct {
	ProviderType       string // Type() value; defaults to "fake"
	Steps              []Step
	Default            *llm.CompletionResponse
	NoStructuredOutput bool // report no structured-output support (exercises the unrouted guide fallback)

	mu       sync.Mutex
	calls    int
	Requests []llm.CompletionRequest // every request received, in order
}

// Text returns a step holding a plain text response.
func Text(content string) Step {
	return Step{Resp: &llm.CompletionResponse{
		Content:    content,
		StopReason: "end_turn",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
}

// Calls returns a step holding tool calls.
func Calls(tcs ...llm.ToolCall) Step {
	return Step{Resp: &llm.CompletionResponse{
		ToolCalls:  tcs,
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
}

// CallsWithText returns a step holding tool calls alongside narration, the
// common real shape (a model explains itself in the same turn it calls a tool).
func CallsWithText(content string, tcs ...llm.ToolCall) Step {
	return Step{Resp: &llm.CompletionResponse{
		Content:    content,
		ToolCalls:  tcs,
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
}

// Truncated returns a step whose text was cut by the output cap, the trigger
// for a "continue where you left off" re-prompt.
func Truncated(content string) Step {
	return Step{Resp: &llm.CompletionResponse{
		Content:    content,
		StopReason: "max_tokens",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 64},
	}}
}

// Empty returns a step that spent output tokens but produced neither content
// nor a tool call - the model reasoned itself into silence.
func Empty() Step {
	return Step{Resp: &llm.CompletionResponse{
		StopReason: "end_turn",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 64},
	}}
}

// Silent returns the other empty shape: no content, no tool call, and
// essentially no output tokens either - the provider anomaly that reads like a
// deliberate stop but is not one. The engine must treat it as an empty turn,
// not as "the model is finished" (see handleNoToolCalls).
func Silent() Step {
	return Step{Resp: &llm.CompletionResponse{
		StopReason: "end_turn",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 0},
	}}
}

// Fail returns a step holding an error.
func Fail(err error) Step { return Step{Err: err} }

func (p *Provider) Name() string { return "enginetest" }

func (p *Provider) Type() string {
	if p.ProviderType != "" {
		return p.ProviderType
	}
	return "fake"
}

// CallCount returns how many Complete calls the provider has served.
func (p *Provider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *Provider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Requests = append(p.Requests, req)
	i := p.calls
	p.calls++
	if i < len(p.Steps) {
		s := p.Steps[i]
		if s.Err != nil {
			return nil, s.Err
		}
		return s.Resp, nil
	}
	if p.Default != nil {
		return p.Default, nil
	}
	return &llm.CompletionResponse{Content: "done", StopReason: "end_turn"}, nil
}

func (p *Provider) Stream(ctx context.Context, req llm.CompletionRequest, cb llm.StreamCallback) error {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return err
	}
	cb(llm.StreamChunk{Delta: resp.Content, Usage: &resp.Usage})
	return nil
}

func (p *Provider) Ping(context.Context, string) error { return nil }

// SupportsStructuredOutput satisfies llm.StructuredOutputProvider so the plan
// stage's schema gate passes in offline tests. Set NoStructuredOutput to script
// a provider that cannot force JSON.
func (p *Provider) SupportsStructuredOutput() bool { return !p.NoStructuredOutput }
