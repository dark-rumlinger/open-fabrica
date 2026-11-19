package engine

import (
	"context"
	"testing"

	"openfabrica/providers/llm"
	"openfabrica/store"
)

func TestVisualDefectIssuesFiltersAndCaps(t *testing.T) {
	findings := []visualFinding{
		{Page: "/", Viewport: "desktop", Severity: "warning", Issue: "minor nitpick"},
		{Page: "/", Viewport: "mobile", Severity: "error", Issue: "HUD overlaps score", Fix: "add top padding"},
		{Page: "/play", Viewport: "desktop", Severity: "error", Issue: "canvas is blank black"},
		{Page: "/play", Viewport: "mobile", Severity: "error", Issue: "overflow"},
		{Page: "/a", Viewport: "desktop", Severity: "error", Issue: "err 4"},
		{Page: "/b", Viewport: "desktop", Severity: "error", Issue: "err 5"},
		{Page: "/c", Viewport: "desktop", Severity: "error", Issue: "err 6 (should be capped)"},
	}
	got := visualDefectIssues(findings)
	if len(got) != maxVisualDefects {
		t.Fatalf("got %d issues, want %d", len(got), maxVisualDefects)
	}
	if got[0].Category != visualDefectCategory || got[0].Severity != store.SeverityError || !got[0].Deterministic {
		t.Errorf("unexpected first issue: %+v", got[0])
	}
	if got[0].Page != "/" || got[0].Message != "[mobile] HUD overlaps score Fix: add top padding" {
		t.Errorf("unexpected formatted issue: %+v", got[0])
	}
}

func TestParseVisualAuditToleratesFences(t *testing.T) {
	raw := "```json\n{\"items\":[{\"page\":\"/\",\"viewport\":\"desktop\",\"severity\":\"error\",\"issue\":\"overlapping nav\",\"fix\":\"add gap\"}]}\n```"
	got, err := parseVisualAudit(raw)
	if err != nil {
		t.Fatalf("parseVisualAudit: %v", err)
	}
	if len(got) != 1 || got[0].Issue != "overlapping nav" {
		t.Fatalf("unexpected parsed findings: %+v", got)
	}
}

func TestSupportsVisionGate(t *testing.T) {
	// Ollama with CapVision=Yes now supports vision on both compat and native paths.
	ollamaKey := llm.ModelKey{Provider: "local-ollama", Model: "llava:latest"}
	llm.Caps.Declare(ollamaKey, llm.CapVision, llm.Yes)
	if !supportsVision(resolvedProvider{Provider: fakeVisionProvider{name: "local-ollama", ptype: "ollama"}, Model: "llava:latest"}) {
		t.Error("expected ollama llava:latest with CapVision=Yes to support vision")
	}

	// OpenAI/Anthropic with CapVision=Yes returns true; Unknown/No returns false.
	visionKey := llm.ModelKey{Provider: "openai", Model: "gpt-4o-test"}
	llm.Caps.Declare(visionKey, llm.CapVision, llm.Yes)
	if !supportsVision(resolvedProvider{Provider: fakeVisionProvider{name: "openai", ptype: "openai"}, Model: "gpt-4o-test"}) {
		t.Error("expected gpt-4o-test with CapVision=Yes to support vision")
	}
	noVisionKey := llm.ModelKey{Provider: "openai", Model: "text-only-test"}
	llm.Caps.Declare(noVisionKey, llm.CapVision, llm.No)
	if supportsVision(resolvedProvider{Provider: fakeVisionProvider{name: "openai", ptype: "openai"}, Model: "text-only-test"}) {
		t.Error("expected text-only-test with CapVision=No not to support vision")
	}
}

func TestRunVisualAuditRespectsMaxCap(t *testing.T) {
	w := &Worker{build: &buildState{visualAudits: maxVisualAudits}}
	issues, ok := w.runVisualAudit(context.Background())
	if ok || len(issues) != 0 {
		t.Fatalf("expected skip at maxVisualAudits, got ok=%v issues=%v", ok, issues)
	}
}

type fakeVisionProvider struct {
	name  string
	ptype string
}

func (f fakeVisionProvider) Name() string { return f.name }
func (f fakeVisionProvider) Type() string { return f.ptype }
func (f fakeVisionProvider) Complete(context.Context, llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return nil, nil
}
func (f fakeVisionProvider) Stream(context.Context, llm.CompletionRequest, llm.StreamCallback) error {
	return nil
}
func (f fakeVisionProvider) Ping(context.Context, string) error { return nil }
