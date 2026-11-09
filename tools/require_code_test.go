package tools

import "testing"

// A missing required argument must surface the SAME machine-readable code
// (CodeMissingArg) no matter which layer caught it - the schema check
// (validateArgs, covered in validate_args_test.go) and the RequireAction
// hand-check. This pins the helper to the shared code, so an identical
// mistake never reports differently depending on which layer caught it.
func TestRequireHelpersCarryMissingArgCode(t *testing.T) {
	if _, r := RequireAction(map[string]any{}); r == nil || r.Code != CodeMissingArg {
		t.Fatalf("RequireAction: want Code=%q, got %+v", CodeMissingArg, r)
	}
	// A supplied value must NOT be flagged.
	if _, r := RequireAction(map[string]any{"action": "save"}); r != nil {
		t.Fatalf("RequireAction on a present value should pass, got %+v", r)
	}
}
