package tools

import (
	"strings"
	"testing"

	"openfabrica/providers/llm"
)

// TestExplainUnavailable pins the find_tool legibility contract: a registered
// tool that's gated on a missing capability returns an actionable reason (not a
// bare not_found), while an available or unknown tool returns "".
func TestExplainUnavailable(t *testing.T) {
	r := NewRegistry()
	r.tools["needs_embed"] = &ToolDef{Name: "needs_embed", OneLiner: "x", RequiresCapability: "embeddings"}
	r.tools["live_only"] = &ToolDef{Name: "live_only", OneLiner: "x", Stages: []string{"live"}}
	r.tools["plain"] = &ToolDef{Name: "plain", OneLiner: "x"}

	noEmbed := llm.Capabilities{}
	withEmbed := llm.Capabilities{HasEmbeddings: true}

	reason := r.ExplainUnavailable("needs_embed", noEmbed, "build")
	if reason == "" || !strings.Contains(reason, "embeddings") || !strings.Contains(reason, "manage_chat") {
		t.Errorf("embeddings-gated reason should name embeddings + manage_chat, got %q", reason)
	}
	// Once embeddings exist, the same tool is loadable -> no reason.
	if r.ExplainUnavailable("needs_embed", withEmbed, "build") != "" {
		t.Error("tool should be loadable when its capability is present")
	}
	// Stage-gated tool reports a stage reason.
	if reason := r.ExplainUnavailable("live_only", withEmbed, "build"); reason == "" || !strings.Contains(reason, "stage") {
		t.Errorf("stage-gated reason should mention stage, got %q", reason)
	}
	// Unknown name -> "" (caller treats as not_found).
	if r.ExplainUnavailable("does_not_exist", withEmbed, "build") != "" {
		t.Error("unknown tool should return empty reason")
	}
	// Plain available tool -> "".
	if r.ExplainUnavailable("plain", withEmbed, "build") != "" {
		t.Error("plain available tool should return empty reason")
	}
}
