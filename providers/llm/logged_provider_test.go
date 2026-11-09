package llm

import (
	"context"
	"testing"
)

// stubProvider is a Provider that answers every call from fixed fields.
type stubProvider struct {
	resp CompletionResponse
}

func (s *stubProvider) Name() string                       { return "stub" }
func (s *stubProvider) Type() string                       { return "stub-transport" }
func (s *stubProvider) Ping(context.Context, string) error { return nil }

func (s *stubProvider) Complete(context.Context, CompletionRequest) (*CompletionResponse, error) {
	r := s.resp
	return &r, nil
}

func (s *stubProvider) Stream(ctx context.Context, req CompletionRequest, cb StreamCallback) error {
	cb(StreamChunk{Delta: s.resp.Content, Usage: &s.resp.Usage})
	return nil
}

// captureLogger records the entries a LoggedProvider emits.
type captureLogger struct {
	entries []LLMLogEntry
}

func (c *captureLogger) LogLLMCall(entry LLMLogEntry) { c.entries = append(c.entries, entry) }

// TestLoggedProviderNilCursors covers the callers outside the build loop, which
// pass nil for both the stage pointer and the iteration counter: the entry must
// be built with zero values rather than faulting. Every non-engine source (chat,
// job, function, proxy) logs through this path, so a deref here takes down the
// live request that made the call.
func TestLoggedProviderNilCursors(t *testing.T) {
	inner := &stubProvider{resp: CompletionResponse{
		Content:    `{"ok":true}`,
		Model:      "m",
		StopReason: "end",
		Usage:      Usage{InputTokens: 11, OutputTokens: 7},
	}}
	logger := &captureLogger{}
	lp := NewLoggedProvider(inner, logger, "proxy", "generate", nil, nil)

	resp, err := lp.Complete(context.Background(), CompletionRequest{Model: "m", MaxTokens: 4096})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != `{"ok":true}` {
		t.Errorf("Content = %q, want the inner provider's reply", resp.Content)
	}
	if len(logger.entries) != 1 {
		t.Fatalf("logged %d entries, want 1", len(logger.entries))
	}
	e := logger.entries[0]
	if e.Stage != "" || e.Iteration != 0 {
		t.Errorf("stage/iteration = %q/%d, want empty/0 for a caller with no cursors", e.Stage, e.Iteration)
	}
	if e.Source != "proxy" || e.SessionID != "generate" {
		t.Errorf("source/session = %q/%q, want proxy/generate", e.Source, e.SessionID)
	}
	if e.ResponseContent != `{"ok":true}` || e.ResponseStopReason != "end" {
		t.Errorf("response not captured: content=%q stop=%q", e.ResponseContent, e.ResponseStopReason)
	}
	if e.InputTokens != 11 || e.OutputTokens != 7 {
		t.Errorf("usage not captured: in=%d out=%d", e.InputTokens, e.OutputTokens)
	}
	if e.ProviderType != "stub-transport" {
		t.Errorf("provider_type = %q, want the inner transport", e.ProviderType)
	}
}

// TestLoggedProviderStreamNilCursors covers the same nil cursors on the
// streaming path, which the proxy's /chat mode takes.
func TestLoggedProviderStreamNilCursors(t *testing.T) {
	inner := &stubProvider{resp: CompletionResponse{Content: "hello", Usage: Usage{InputTokens: 3, OutputTokens: 2}}}
	logger := &captureLogger{}
	lp := NewLoggedProvider(inner, logger, "proxy", "assistant", nil, nil)

	var got string
	if err := lp.Stream(context.Background(), CompletionRequest{Model: "m"}, func(c StreamChunk) { got += c.Delta }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got != "hello" {
		t.Errorf("streamed %q, want hello", got)
	}
	if len(logger.entries) != 1 {
		t.Fatalf("logged %d entries, want 1", len(logger.entries))
	}
	if e := logger.entries[0]; e.ResponseContent != "hello" || e.OutputTokens != 2 {
		t.Errorf("stream entry not captured: content=%q out=%d", e.ResponseContent, e.OutputTokens)
	}
}
