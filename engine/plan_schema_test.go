package engine

// planSchemaJSON is a hand-maintained JSON document embedded in a Go raw-string
// literal. That is a sharp edge: a stray backtick truncates the literal, and an
// in-place edit can silently drop a sibling property. These tests parse it as real
// JSON and assert the task-item property set, so either mistake fails loudly here
// instead of shipping a malformed response schema to providers.

import (
	"encoding/json"
	"testing"
)

func TestPlanSchemaJSONIsValidAndComplete(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(planSchemaJSON), &root); err != nil {
		t.Fatalf("planSchemaJSON is not valid JSON: %v", err)
	}

	// Drill to the task item's properties: root.properties.tasks.items.properties.
	props, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties object")
	}
	tasks, ok := props["tasks"].(map[string]any)
	if !ok {
		t.Fatal("schema has no tasks property")
	}
	items, ok := tasks["items"].(map[string]any)
	if !ok {
		t.Fatal("tasks has no items")
	}
	taskProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatal("task items has no properties")
	}

	// Every field the seed path reads must be declared, so a grammar-constrained
	// provider can emit it. `exports` and `tools` are the pair most at risk: they sit
	// adjacent and an edit to one has already clobbered the other once.
	for _, field := range []string{"id", "title", "intent", "done_when", "deps", "paths", "edits", "exports", "tools"} {
		if _, ok := taskProps[field].(map[string]any); !ok {
			t.Errorf("task schema is missing property %q (a dropped property means the planner can never emit it)", field)
		}
	}
}
