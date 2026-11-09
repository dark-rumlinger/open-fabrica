package tools

import (
	"strings"
	"testing"
)

func inferTestDef() *ToolDef {
	return &ToolDef{
		Name: "manage_widget",
		Params: []Param{
			{Name: "action", Type: "string", Required: true, Enum: []string{"create", "get", "delete", "list"}},
			{Name: "name", Type: "string"},
			{Name: "code", Type: "string"},
		},
		ActionMeta: map[string]ActionMeta{
			"create": {Params: []Param{
				{Name: "name", Type: "string", Required: true},
				{Name: "code", Type: "string", Required: true},
			}},
			"get": {Params: []Param{
				{Name: "name", Type: "string", Required: true},
			}},
			"delete": {Params: []Param{
				{Name: "name", Type: "string", Required: true},
			}},
			"list": {Params: nil},
		},
	}
}

// inferMissingAction fills a missing action only when the argument shape fits
// exactly one action; any ambiguity keeps the normal missing_arg rejection.
func TestInferMissingAction(t *testing.T) {
	def := inferTestDef()

	// name+code fits only create (get/delete do not declare code; list has no
	// required params and never infers).
	args := map[string]any{"name": "fn", "code": "x()"}
	if got := inferMissingAction(def, args); got != "create" || args["action"] != "create" {
		t.Fatalf("inferMissingAction = %q (action=%v), want create", got, args["action"])
	}

	// name alone fits both get and delete: ambiguous, no inference.
	args = map[string]any{"name": "fn"}
	if got := inferMissingAction(def, args); got != "" {
		t.Fatalf("ambiguous shape inferred %q, want no inference", got)
	}
	if _, set := args["action"]; set {
		t.Fatal("ambiguous inference must not set action")
	}

	// A supplied arg no action declares blocks inference.
	args = map[string]any{"name": "fn", "code": "x()", "bogus": 1}
	if got := inferMissingAction(def, args); got != "" {
		t.Fatalf("undeclared arg should block inference, got %q", got)
	}

	// An explicit action is never overridden.
	args = map[string]any{"action": "get", "name": "fn", "code": "x()"}
	if got := inferMissingAction(def, args); got != "" || args["action"] != "get" {
		t.Fatalf("explicit action must pass through, got inferred=%q action=%v", got, args["action"])
	}

	// Empty args never infer (a bare call is not evidence of anything).
	if got := inferMissingAction(def, map[string]any{}); got != "" {
		t.Fatalf("empty args inferred %q", got)
	}
}

// End to end through Execute: an unambiguous shape dispatches with a notice;
// an ambiguous one still gets the missing_arg rejection.
func TestExecuteInfersMissingAction(t *testing.T) {
	def := inferTestDef()
	var ran string
	handler := func(name string) ActionHandler {
		return func(ctx *ToolContext, args map[string]any) (*Result, error) {
			ran = name
			return Ok(map[string]any{"ok": true}), nil
		}
	}
	def.Actions = map[string]ActionHandler{
		"create": handler("create"), "get": handler("get"),
		"delete": handler("delete"), "list": handler("list"),
	}

	res, err := def.Execute(&ToolContext{}, map[string]any{"name": "fn", "code": "x()"})
	if err != nil || !res.Success {
		t.Fatalf("Execute: err=%v res=%+v", err, res)
	}
	if ran != "create" {
		t.Fatalf("dispatched %q, want create", ran)
	}
	notice, _ := res.Data.(map[string]any)["notice"].(string)
	if !strings.Contains(notice, "inferred") {
		t.Errorf("result should carry the inference notice, got %q", notice)
	}

	res, err = def.Execute(&ToolContext{}, map[string]any{"name": "fn"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Success || res.Code != CodeMissingArg {
		t.Fatalf("ambiguous shape should keep the missing_arg rejection, got %+v", res)
	}
}
