package llm

import (
	"context"
	"errors"
	"testing"
)

// probeFake streams a fixed chunk sequence, answers Complete with a fixed response,
// and records whether thinking was asked for.
type probeFake struct {
	chunks   []StreamChunk
	resp     *CompletionResponse
	err      error
	thinking *bool
}

func (f *probeFake) Name() string { return "fake" }
func (f *probeFake) Type() string { return "openai-chat" }
func (f *probeFake) Complete(_ context.Context, req CompletionRequest) (*CompletionResponse, error) {
	f.thinking = req.Thinking
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}
func (f *probeFake) Ping(context.Context, string) error { return nil }
func (f *probeFake) Stream(ctx context.Context, req CompletionRequest, cb StreamCallback) error {
	f.thinking = req.Thinking
	if f.err != nil {
		return f.err
	}
	for _, c := range f.chunks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cb(c)
	}
	return nil
}

func TestProbeReasoning(t *testing.T) {
	for _, c := range []struct {
		name string
		fake *probeFake
		want ProbeVerdict
	}{
		{"reasoning channel first", &probeFake{chunks: []StreamChunk{{Reasoning: "17 plus 25"}, {Delta: "42"}}}, ProbeYes},
		{"direct answer", &probeFake{chunks: []StreamChunk{{Delta: "42"}}}, ProbeNo},
		{"longer direct answer", &probeFake{chunks: []StreamChunk{{Delta: "The answer"}, {Delta: " is 42"}}}, ProbeNo},
		{"inline think block", &probeFake{chunks: []StreamChunk{{Delta: "<thi"}, {Delta: "nk>add them"}}}, ProbeYes},
		{"provider error", &probeFake{err: errors.New("connection reset")}, ProbeUnknown},
		{"empty stream", &probeFake{}, ProbeUnknown},
	} {
		if got, _ := ProbeReasoning(context.Background(), c.fake, "m"); got != c.want {
			t.Errorf("%s: verdict %v, want %v", c.name, got, c.want)
		}
		if c.fake.err == nil && (c.fake.thinking == nil || !*c.fake.thinking) {
			t.Errorf("%s: the reasoning probe must ask for thinking", c.name)
		}
	}
}

func TestProbeTools(t *testing.T) {
	call := &CompletionResponse{ToolCalls: []ToolCall{{Name: "add_numbers", Arguments: `{"a":17,"b":25}`}}}
	prose := &CompletionResponse{Content: "17 + 25 = 42"}
	for _, c := range []struct {
		name string
		fake *probeFake
		want ProbeVerdict
	}{
		{"calls the tool", &probeFake{resp: call}, ProbeYes},
		{"answers in prose", &probeFake{resp: prose}, ProbeNo},
		{"refuses the tools field", &probeFake{err: &Error{Code: CodeBadRequest, Status: 400}}, ProbeNo},
		{"rate limited", &probeFake{err: &Error{Code: CodeRateLimit, Status: 429}}, ProbeUnknown},
	} {
		got, err := ProbeTools(context.Background(), c.fake, "m")
		if got != c.want {
			t.Errorf("%s: verdict %v, want %v", c.name, got, c.want)
		}
		if c.name == "rate limited" && !IsRateLimited(err) {
			t.Errorf("%s: the error must read as rate limited so a batch stops", c.name)
		}
		if c.fake.thinking == nil || *c.fake.thinking {
			t.Errorf("%s: the tools probe must turn thinking off", c.name)
		}
	}
}
