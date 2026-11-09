package engine

// Hints-prose consistency guards.
//
// A tool's Hints is LLM-facing prose like any other - the executor appends its
// return to the tool result - but it is a func, not a string, so every other
// drift guard misses it: nothing rendered a Hints body for inspection. These
// guards render every Hints body - per declared action, with args built from
// the action's own schema - and feed the output through the same checks. The
// coverage assertion keeps it honest: a harness that silently rendered nothing
// would pass every content check vacuously.

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// hintArgsFor builds a plausible args map for one action from its declared
// params, so a Hints body that reads args (endpoint_path, table_name, ...) takes
// its real branch instead of bailing early on an empty map.
func hintArgsFor(action string, params []tools.Param) map[string]any {
	args := map[string]any{"action": action}
	for _, p := range params {
		if _, ok := args[p.Name]; ok {
			continue
		}
		switch {
		case len(p.Enum) > 0:
			args[p.Name] = p.Enum[0]
		case p.Type == "integer" || p.Type == "number":
			args[p.Name] = 1
		case p.Type == "boolean":
			args[p.Name] = true
		case p.Type == "array":
			args[p.Name] = []any{"x"}
		case p.Type == "object":
			args[p.Name] = map[string]any{}
		default:
			args[p.Name] = "x"
		}
	}
	return args
}

// renderHints returns every Hints body a tool can produce, keyed by tool/action
// (the action comes from args["action"], as in tools/executor.go). Both a
// successful and a failed Result are rendered: several bodies branch on it, so
// rendering only one shape would leave the other branch's prose out of the
// corpus (reachability is a separate question, tracked on its own).
func renderHints(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	results := map[string]*tools.Result{
		"":              {Success: true, Data: map[string]any{}},
		" (on failure)": {Success: false, Error: "no embedding provider", Data: map[string]any{}},
	}
	for _, td := range tools.DefaultRegistry().List() {
		if td.Hints == nil {
			continue
		}
		for action := range td.Actions {
			params := td.Params
			if meta, ok := td.ActionMeta[action]; ok && len(meta.Params) > 0 {
				params = meta.Params
			}
			args := hintArgsFor(action, params)
			for suffix, result := range results {
				hint := func() string {
					// A Hints body that panics on a synthetic arg shape is itself
					// a finding: the executor calls it with whatever the model sent.
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("tool %q Hints(%q) panicked: %v", td.Name, action, r)
						}
					}()
					return td.Hints(action, args, result)
				}()
				if strings.TrimSpace(hint) != "" {
					out[td.Name+"/"+action+suffix] = hint
				}
			}
		}
	}
	return out
}

// TestHintsProseIsReachable is the coverage assertion the content checks below
// depend on: every tool that declares Hints must yield at least one non-empty
// body. Without it a harness that rendered nothing would pass them all vacuously.
func TestHintsProseIsReachable(t *testing.T) {
	rendered := renderHints(t)
	for _, td := range tools.DefaultRegistry().List() {
		if td.Hints == nil {
			continue
		}
		found := false
		for key := range rendered {
			if strings.HasPrefix(key, td.Name+"/") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tool %q declares Hints but no action rendered a body - the guards below would pass vacuously for it", td.Name)
		}
	}
	if len(rendered) == 0 {
		t.Fatal("no Hints bodies rendered at all - are the tool subpackages imported?")
	}
}

// TestHintsReferenceRealTools is the Hints half of TestDocsReferenceRealTools: a
// manage_* name in a hint that no longer names a registered tool sends the model
// after something that does not exist.
func TestHintsReferenceRealTools(t *testing.T) {
	known := map[string]bool{"find_tool": true}
	for _, td := range tools.DefaultRegistry().List() {
		known[td.Name] = true
	}
	for where, prose := range renderHints(t) {
		for _, name := range toolToken.FindAllString(prose, -1) {
			if !known[name] {
				t.Errorf("%s hint references %q, which is not a registered tool", where, name)
			}
		}
	}
}

// A `window.*` content guard over hint prose is deliberately NOT here: the
// corrected text has to NAME the anti-pattern to warn against it, so a
// substring ban trips on the proscription exactly as on the wrong prose. A
// check that cannot tell a prescription from a proscription is a
// false-positive generator. The reachable guard is the tool-name check above.
