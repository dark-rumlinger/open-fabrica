package analysis

import (
	"sort"
	"testing"
)

// The project keeps TWO import-graph builders on purpose: BuildModuleGraph
// (regex, resilient reachability) and AnalyzeModules -> facts.Edges (esbuild,
// resolution correctness). These tests pin the boundary: the builders AGREE on
// well-formed modules and DIVERGE exactly on unparseable input. Converging the
// two would change a verdict on broken input, so they stay separate.

// edgeSet reduces either graph to importer -> sorted target list for comparison.
func factsEdgeSet(f *ModuleFacts) map[string][]string {
	out := map[string][]string{}
	for from, edges := range f.Edges {
		for _, e := range edges {
			out[from] = append(out[from], e.To)
		}
		sort.Strings(out[from])
	}
	return out
}

func regexEdgeSet(g ModuleGraph) map[string][]string {
	out := map[string][]string{}
	for from, tos := range g.Edges {
		cp := append([]string(nil), tos...)
		sort.Strings(cp)
		out[from] = cp
	}
	return out
}

func sameEdgeSets(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
	}
	return true
}

func TestImportGraphs_AgreeOnWellFormed(t *testing.T) {
	bodies := map[string]string{
		"js/app.js":   "import { render } from './view.js';\nimport { state } from './state.js';\nimport('./lazy.js').then(m => m.go());\nrender(state);",
		"js/view.js":  "export function render(s){ return s; }",
		"js/state.js": "export const state = {};",
		"js/lazy.js":  "export function go(){}",
	}
	regex := regexEdgeSet(BuildModuleGraph(bodies))
	esbuild := factsEdgeSet(AnalyzeModules(bodies))
	if !sameEdgeSets(regex, esbuild) {
		t.Errorf("well-formed graphs must agree on resolvable edges (static + literal dynamic).\n regex   = %v\n esbuild = %v", regex, esbuild)
	}
	// Sanity: the edges we expect are actually present.
	if got := regex["js/app.js"]; len(got) != 3 {
		t.Errorf("app.js should import view, state, lazy; got %v", got)
	}
}

func TestImportGraphs_DivergeOnUnparseableInput(t *testing.T) {
	// app.js has a syntax error but a resolvable import. esbuild cannot parse it, so
	// it captures no app->view edge; the regex builder still sees the import
	// statement. This is the intended, documented divergence.
	bodies := map[string]string{
		"js/app.js":  "import { render } from './view.js';\nfunction broken( {", // unbalanced
		"js/view.js": "export function render(){}",
	}
	regex := regexEdgeSet(BuildModuleGraph(bodies))
	esbuild := factsEdgeSet(AnalyzeModules(bodies))
	if len(regex["js/app.js"]) == 0 {
		t.Fatal("regex builder should stay resilient and keep the app->view edge on a broken module")
	}
	if len(esbuild["js/app.js"]) != 0 {
		t.Fatalf("esbuild is expected to drop a broken module's outgoing edges; got %v - if this now matches the regex builder, the two can be converged (revisit the deferred item)", esbuild["js/app.js"])
	}
}
