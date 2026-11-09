package engine

import (
	"testing"

	"openfabrica/providers/llm"
)

// A red replay of a saved script is the replay's verdict on the app, not a rejected
// edit, and neither is a refused rewrite into diagnostics; a parse reject and other
// tools still are.
func TestIsFlowScriptVerdict(t *testing.T) {
	call := llm.ToolCall{Name: "manage_testing"}
	write := map[string]any{"action": "write_flows"}
	cases := []struct {
		name   string
		tc     llm.ToolCall
		args   map[string]any
		result string
		want   bool
	}{
		{"red replay of a saved script", call, write,
			`{"success":false,"error":"1 of 2 flows failed: a","data":{"saved":"of_test.js","failed":["a"]}}`, true},
		{"refused diagnostic rewrite", call, write,
			`{"success":false,"code":"diagnostic_suite","error":"not saved"}`, true},
		{"parse reject saved nothing", call, write,
			`{"success":false,"error":"flow \"a\" step 1: has more than one verb"}`, false},
		{"a test_flows call", call, map[string]any{"action": "test_flows"},
			`{"success":false,"data":{"saved":"of_test.js"}}`, false},
		{"script edit routed through manage_files", llm.ToolCall{Name: "manage_files"}, map[string]any{"action": "patch"},
			`{"success":false,"error":"1 of 2 flows failed: a","data":{"saved":"of_test.js","routed":"x"}}`, true},
		{"an ordinary rejected manage_files edit", llm.ToolCall{Name: "manage_files"}, map[string]any{"action": "patch"},
			`{"success":false,"code":"patch_anchor_not_found","error":"x"}`, false},
		{"another tool", llm.ToolCall{Name: "manage_function"}, write,
			`{"success":false,"data":{"saved":"x"}}`, false},
	}
	for _, c := range cases {
		if got := isFlowScriptVerdict(c.tc, c.args, c.result); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
