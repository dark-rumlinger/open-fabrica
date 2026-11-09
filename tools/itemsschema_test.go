package tools

import (
	"reflect"
	"testing"
)

// The wire schema and the arg description are read by the same model in the same breath,
// and when they disagree the schema wins - it is a constraint, the prose is advice. This
// file guards the one place they systematically disagreed.
//
// The Go kind is a poor witness for what an array arg accepts. Any arg that must tolerate
// loose model output is typed []any, which reflects to element kind Interface, which
// itemsSchemaFor can only render as {"type":"object"}. Without a tag, every string-list arg
// in the registry advertises object items, even one like manage_auth's public_columns whose
// description calls for "a bare array of strings ... NOT objects". The `items:"..."` tag
// (see parseItemsTag) lets the field stay []any while the schema states the truth.
//
// These are the tag MECHANICS. The registry-wide sweep that catches an untagged string list
// lives in engine/tool_items_schema_test.go, because only the engine test binary imports
// every tool subpackage - this package cannot, it is the one they all import.

// TestItemsTagOverridesGoKind is the positive control for the tag itself: it has to reach
// the rendered schema, or the registry sweep passes because nothing is tagged rather than
// because everything is correct.
func TestItemsTagOverridesGoKind(t *testing.T) {
	def := defineFixture(t, Tool{
		Name: "manage_items_fixture", Category: "test",
		OneLiner: "Items-tag rendering fixture.", WhenToUse: "Test-only.",
		Actions: []Action{&fakeItems{}},
	})
	props, _ := def.Parameters()["properties"].(map[string]any)

	for arg, wantType := range map[string]string{
		"names":    "string",
		"ids":      "integer",
		"untagged": "object", // no tag: the Go kind still decides
	} {
		prop, ok := props[arg].(map[string]any)
		if !ok {
			t.Fatalf("arg %q missing from rendered schema", arg)
		}
		items, ok := prop["items"].(map[string]any)
		if !ok {
			t.Fatalf("arg %q rendered no items schema", arg)
		}
		if got, _ := items["type"].(string); got != wantType {
			t.Errorf("arg %q items type = %q, want %q", arg, got, wantType)
		}
	}

	// "any" means no element constraint: an empty items schema, never a wrong one.
	prop, _ := props["mixed"].(map[string]any)
	if items, _ := prop["items"].(map[string]any); items == nil || len(items) != 0 {
		t.Errorf(`arg "mixed" carries items %v; items:"any" must emit an empty schema`, prop["items"])
	}
}

// TestItemsTagRejectsBadValues pins the two registration-time refusals, so a typo is a
// boot failure rather than a silently dropped constraint.
func TestItemsTagRejectsBadValues(t *testing.T) {
	type badType struct {
		N string `arg:"n" desc:"d" items:"string"`
	}
	if _, err := extractActionFields(reflect.TypeOf(badType{})); err == nil {
		t.Error("items tag on a non-array arg was accepted")
	}
	type badValue struct {
		N []any `arg:"n" desc:"d" items:"strings"`
	}
	if _, err := extractActionFields(reflect.TypeOf(badValue{})); err == nil {
		t.Error(`items:"strings" (not a JSON Schema type) was accepted`)
	}
}

// fakeItems exercises every items-tag path for the rendering test.
type fakeItems struct {
	Names    []any `arg:"names"    desc:"Column names."           items:"string"`
	IDs      []any `arg:"ids"      desc:"Row ids."               items:"integer"`
	Mixed    []any `arg:"mixed"    desc:"Positional arguments."  items:"any"`
	Untagged []any `arg:"untagged" desc:"Structured filter list."`
}

func (a *fakeItems) Doc() string                           { return "Items-tag fixture." }
func (a *fakeItems) Run(ctx *ToolContext) (*Result, error) { return &Result{Success: true}, nil }

// TestItemsSchemaHookReplacesDefault pins the ItemsSchema hook: an action that returns a
// schema for an array arg gets exactly that schema on the wire, and a nil return keeps the
// Go-kind default for the others.
func TestItemsSchemaHookReplacesDefault(t *testing.T) {
	def := defineFixture(t, Tool{
		Name: "manage_items_hook_fixture", Category: "test",
		OneLiner: "Items-hook rendering fixture.", WhenToUse: "Test-only.",
		Actions: []Action{&fakeItemsHook{}},
	})
	props, _ := def.Parameters()["properties"].(map[string]any)

	hunks, _ := props["hunks"].(map[string]any)
	items, _ := hunks["items"].(map[string]any)
	fields, _ := items["properties"].(map[string]any)
	if fields["search"] == nil {
		t.Fatalf("hook schema did not reach the wire, hunks.items = %v", hunks["items"])
	}
	other, _ := props["other"].(map[string]any)
	if got, _ := other["items"].(map[string]any)["type"].(string); got != "object" {
		t.Errorf("a nil hook return must keep the default items schema, got %v", other["items"])
	}
}

// fakeItemsHook gives one array arg a full items schema through the hook.
type fakeItemsHook struct {
	Hunks []map[string]any `arg:"hunks" desc:"Edit hunks."`
	Other []map[string]any `arg:"other" desc:"Untouched list."`
}

func (a *fakeItemsHook) Doc() string                           { return "Items-hook fixture." }
func (a *fakeItemsHook) Run(ctx *ToolContext) (*Result, error) { return &Result{Success: true}, nil }
func (a *fakeItemsHook) ItemsSchema(arg string) map[string]any {
	if arg != "hunks" {
		return nil
	}
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"search": map[string]any{"type": "string"}},
	}
}
