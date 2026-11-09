package tools

import (
	"context"
	"testing"

	"openfabrica/providers/llm"
)

// With embeddings on, a query nothing resembles must still come back as a miss with
// guidance, not as a list of every tool ranked by noise.
func TestServiceFindTool_SemanticMissStillReportsAMiss(t *testing.T) {
	r := NewRegistry()
	newTestTool(r, "manage_email", "messaging", "Send transactional email.", "Use for email.", "", "send", "deliver an email")
	newTestTool(r, "manage_feeds", "content", "Publish RSS feeds.", "Use for feeds.", "", "create", "create a feed")

	// Catalogue texts and the probe land on the first axis; the query is orthogonal.
	embed := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			if text == "zzz unrelated gibberish" {
				out[i] = []float32{0, 1}
			} else {
				out[i] = []float32{1, 0}
			}
		}
		return out, nil
	}
	caps := llm.Capabilities{HasLLM: true, HasEmbeddings: true}
	res, _ := ServiceFindTool(context.Background(), r, caps, "build",
		FindToolArgs{Query: "zzz unrelated gibberish"}, func(string) bool { return false }, embed)
	if len(res.Matches) != 0 {
		t.Fatalf("an unrelated query matched %d tools, want a miss", len(res.Matches))
	}
	if res.Note == "" && len(res.Unavailable) == 0 {
		t.Error("a miss must carry guidance (a note or the tools gated off), not a bare success")
	}
}
