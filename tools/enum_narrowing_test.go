package tools

import (
	"strings"
	"testing"
)

// The tool-level wire schema unions enums across actions (mergeParam), so the
// model can see a value the chosen action rejects. enumNarrowingDoc surfaces
// each action's real accepted set in the find_tool doc; these tests pin the
// rendering and the differs-from-union condition.

func narrowingToolDef() *ToolDef {
	return &ToolDef{
		Name: "manage_demo",
		Params: []Param{
			{Name: "action", Type: "string", Enum: []string{"read", "write"}},
			{Name: "mode", Type: "string", Enum: []string{"plain", "append", "overwrite"}},
		},
		ActionMeta: map[string]ActionMeta{
			"read": {Params: []Param{
				{Name: "mode", Type: "string", Enum: []string{"plain"}},
			}},
			"write": {Params: []Param{
				{Name: "mode", Type: "string", Enum: []string{"plain", "append", "overwrite"}},
			}},
		},
		Actions: map[string]ActionHandler{"read": nil, "write": nil},
	}
}

func TestEnumNarrowingDocListsNarrowedActions(t *testing.T) {
	doc := narrowingToolDef().enumNarrowingDoc()
	if !strings.Contains(doc, "`manage_demo.read` accepts only: mode=[plain]") {
		t.Errorf("doc should surface the read action's narrowed enum, got %q", doc)
	}
	if strings.Contains(doc, "manage_demo.write") {
		t.Errorf("write matches the union - it must not be listed, got %q", doc)
	}
}

func TestEnumNarrowingDocEmptyWhenNothingNarrows(t *testing.T) {
	d := narrowingToolDef()
	d.ActionMeta["read"] = ActionMeta{Params: []Param{
		{Name: "mode", Type: "string", Enum: []string{"plain", "append", "overwrite"}},
	}}
	if doc := d.enumNarrowingDoc(); doc != "" {
		t.Errorf("no action narrows the union, doc should be empty, got %q", doc)
	}
}

// A per-action invalid_enum rejection explains WHY the wire schema showed more
// values than the action accepts; every other code passes through untouched.
func TestClarifyActionEnumAppendsContext(t *testing.T) {
	r := clarifyActionEnum(&Result{Success: false, Code: "invalid_enum",
		Error: `arg "mode" must be one of [plain], got "append"`})
	if !strings.Contains(r.Error, "for THIS action") {
		t.Errorf("invalid_enum must gain the union-vs-action context, got %q", r.Error)
	}
	other := clarifyActionEnum(&Result{Success: false, Code: "missing_arg", Error: "x"})
	if other.Error != "x" {
		t.Errorf("non-enum codes must pass through untouched, got %q", other.Error)
	}
	if clarifyActionEnum(nil) != nil {
		t.Error("nil (validation passed) must stay nil")
	}
}
