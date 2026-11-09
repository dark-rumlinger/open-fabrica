package engine

import (
	"testing"

	"openfabrica/providers/llm"
)

func TestTranscriptAppendOnlyStablePrefix(t *testing.T) {
	tr := NewTranscript(
		llm.Message{Role: llm.RoleUser, Content: "seed"},
	)
	tr.Append(llm.Message{Role: llm.RoleAssistant, Content: "reply"})
	tr.Seal()

	// Clean append-only growth: the full sealed prefix is stable, unsealed
	// tail excluded.
	tr.Append(llm.Message{Role: llm.RoleTool, ToolCallID: "x", Content: "result"})
	stableBytes, stableMsgs := tr.StablePrefix()
	if stableMsgs != 2 {
		t.Fatalf("stableMsgs = %d, want 2", stableMsgs)
	}
	if stableBytes <= 0 {
		t.Fatal("stableBytes should be positive")
	}
	if total := tr.TotalBytes(); total <= stableBytes {
		t.Fatalf("TotalBytes (%d) must include the unsealed tail beyond stableBytes (%d)", total, stableBytes)
	}

	// Sealing the tail extends the stable prefix by exactly its bytes.
	preTotal := tr.TotalBytes()
	tr.Seal()
	gotBytes, gotMsgs := tr.StablePrefix()
	if gotMsgs != 3 || gotBytes != preTotal {
		t.Fatalf("after Seal: stable=(%d,%d), want (%d,3)", gotBytes, gotMsgs, preTotal)
	}
}

func TestTranscriptMessagesIsACopy(t *testing.T) {
	tr := NewTranscript(llm.Message{Role: llm.RoleUser, Content: "seed"})
	tr.Seal()
	preBytes, _ := tr.StablePrefix()
	msgs := tr.Messages()
	msgs[0].Content = "mutated copy - much longer than the original seed content"
	if got, _ := tr.StablePrefix(); got != preBytes {
		t.Fatalf("mutating the returned copy must not affect the transcript: %d != %d", got, preBytes)
	}
	if tr.Messages()[0].Content != "seed" {
		t.Fatal("transcript content changed through the returned copy")
	}
}
