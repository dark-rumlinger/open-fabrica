package replaymodel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"openfabrica/internal/replayoutbox"
	"openfabrica/providers/llm"
)

// The stand-in is active exactly while a replay of the project runs, and Resolve
// skips the real lookup then (so an unconfigured model still replays).
func TestResolveUsesTheStandInOnlyDuringAReplay(t *testing.T) {
	const project = 9301
	realCalls := 0
	real := func() (llm.Provider, string, error) {
		realCalls++
		return nil, "", errors.New("no provider configured")
	}

	if _, _, err := Resolve(project, real); err == nil || realCalls != 1 {
		t.Fatalf("outside a replay the real lookup runs, got err=%v calls=%d", err, realCalls)
	}

	end := replayoutbox.Begin(project)
	p, model, err := Resolve(project, real)
	if err != nil || p == nil || model != ModelID || realCalls != 1 {
		t.Fatalf("during a replay the stand-in answers without the real lookup, got %v %q %v calls=%d", p, model, err, realCalls)
	}
	if !ActiveFor(project) || ActiveFor(project+1) {
		t.Fatal("the stand-in is per project")
	}
	end()
	if ActiveFor(project) {
		t.Fatal("it must stop when the replay ends")
	}
}

// A plain request gets the canned text and no tool calls (so an agent loop ends at once);
// a streamed one gets the same text in pieces and then the end.
func TestProviderRepliesWithCannedText(t *testing.T) {
	p := New()
	resp, err := p.Complete(context.Background(), llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
		Tools:    []llm.ToolDef{{Name: "lookup"}},
	})
	if err != nil || resp.Content != Text || len(resp.ToolCalls) != 0 {
		t.Fatalf("got %+v err=%v", resp, err)
	}

	var got strings.Builder
	var done bool
	err = p.Stream(context.Background(), llm.CompletionRequest{}, func(c llm.StreamChunk) {
		got.WriteString(c.Delta)
		done = done || c.Done
	})
	if err != nil || got.String() != Text || !done {
		t.Fatalf("stream gave %q done=%v err=%v", got.String(), done, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Complete(ctx, llm.CompletionRequest{}); err == nil {
		t.Fatal("a cancelled request must fail like a real call")
	}
}

// With a response schema the reply is JSON that satisfies it, so the caller's own
// validation (and its corrective retry) never fires.
func TestSampleJSONSatisfiesTheSchema(t *testing.T) {
	schema := `{
		"type": "object",
		"required": ["title", "score", "tags", "status", "when"],
		"properties": {
			"title": {"type": "string", "minLength": 8},
			"score": {"type": "integer", "minimum": 3},
			"ratio": {"type": ["number", "null"]},
			"tags": {"type": "array", "items": {"type": "string"}, "minItems": 2},
			"status": {"enum": ["open", "closed"]},
			"when": {"type": "string", "format": "date-time"},
			"nested": {"type": "object", "properties": {"ok": {"type": "boolean"}}},
			"either": {"anyOf": [{"type": "string"}, {"type": "number"}]}
		}
	}`
	out := SampleJSON(json.RawMessage(schema))
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, out)
	}
	if s, _ := v["title"].(string); len(s) < 8 {
		t.Errorf("title shorter than minLength: %v", v["title"])
	}
	if n, _ := v["score"].(float64); n < 3 {
		t.Errorf("score below minimum: %v", v["score"])
	}
	if tags, _ := v["tags"].([]any); len(tags) != 2 {
		t.Errorf("tags must honour minItems: %v", v["tags"])
	}
	if v["status"] != "open" {
		t.Errorf("an enum takes its first value: %v", v["status"])
	}
	if s, _ := v["when"].(string); !strings.HasPrefix(s, "2026-") {
		t.Errorf("date-time format: %v", v["when"])
	}
	if nested, _ := v["nested"].(map[string]any); nested["ok"] != true {
		t.Errorf("nested object: %v", v["nested"])
	}
	if v["either"] != "sample" {
		t.Errorf("anyOf takes the first alternative: %v", v["either"])
	}

	// A request that carries the schema gets it back as JSON content.
	resp, _ := New().Complete(context.Background(), llm.CompletionRequest{
		ResponseSchema: &llm.ResponseSchema{Name: "x", Schema: json.RawMessage(schema)},
	})
	if !json.Valid([]byte(resp.Content)) {
		t.Errorf("structured reply must be JSON, got %q", resp.Content)
	}
	if SampleJSON(json.RawMessage("not json")) != "{}" {
		t.Error("an unreadable schema yields an empty object")
	}
}
