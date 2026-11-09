package analysis

import (
	"reflect"
	"sort"
	"testing"
)

// edgeStrings flattens a ModuleFacts graph to "from -> to[ dyn]" lines, sorted,
// for stable comparison.
func edgeStrings(f *ModuleFacts) []string {
	var out []string
	for from, edges := range f.Edges {
		for _, e := range edges {
			s := from + " -> " + e.To
			if e.Dynamic {
				s += " dyn"
			}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// AnalyzeModules resolves the real import graph through the platform resolver,
// distinguishing a string-literal dynamic import() (an edge) from a static one,
// while external library specifiers are not edges.
func TestAnalyzeModules_GraphAndDynamicKind(t *testing.T) {
	bodies := map[string]string{
		"js/main.js": `import { makeWorld } from "./world.js";
import * as THREE from "three";
const lazy = await import("./lazy.js");
makeWorld(); THREE.x();`,
		"js/world.js": `export function makeWorld(){ return 1; }`,
		"js/lazy.js":  `export const LAZY = 1;`,
	}
	f := AnalyzeModules(bodies)
	got := edgeStrings(f)
	want := []string{
		"js/main.js -> js/lazy.js dyn",
		"js/main.js -> js/world.js",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}
}

// The linker follows a re-export chain that terminates in a real export, so a
// consumer importing through a barrel resolves cleanly.
func TestResolveNamedImports_ReexportChainResolves(t *testing.T) {
	bodies := map[string]string{
		"state.js":     `export const tile = 1;`,
		"barrel.js":    `export { tile } from "./state.js";`,
		"consumeok.js": `import { tile } from "./barrel.js"; console.log(tile);`,
	}
	if issues := ResolveNamedImports("consumeok.js", bodies); len(issues) != 0 {
		t.Fatalf("a re-export chain that terminates in a real export must be clean, got %+v", issues)
	}
}

// At the facts level the linker DOES detect a broken re-export chain (a barrel
// re-exporting a name its source never declares), attributed to the re-export
// site. A forward-looking capability the regex layer never had.
func TestAnalyzeModules_DetectsBrokenReexportChain(t *testing.T) {
	bodies := map[string]string{
		"state.js":  `export const tile = 1;`,
		"barrel.js": `export { ghost } from "./state.js";`,
	}
	f := AnalyzeModules(bodies)
	if !f.Missing("barrel.js", "state.js", "ghost") {
		t.Fatalf("linker should detect the broken re-export ghost from state.js; facts=%+v", f.missing)
	}
}

// Stricter mode: a LOCAL export * is fully knowable, so a name absent from the
// whole star chain is flagged (a real bug the conservative era missed), while
// ExportsComplete reports the barrel as complete.
func TestResolveNamedImports_LocalExportStarChainFollowed(t *testing.T) {
	bodies := map[string]string{
		"impl.js":    `export const a = 1;`,
		"barrel.js":  `export * from "./impl.js";`,
		"consume.js": `import { anything } from "./barrel.js"; console.log(anything);`,
	}
	if !AnalyzeModules(bodies).ExportsComplete("barrel.js") {
		t.Fatalf("a barrel whose star resolves to a local module must be complete")
	}
	issues := ResolveNamedImports("consume.js", bodies)
	if len(issues) != 1 || issues[0].Imported != "anything" {
		t.Fatalf("a name absent from a fully-local star chain must be flagged, got %+v", issues)
	}
}

// A target that re-exports from an EXTERNAL library via `export *` is unknowable:
// a name imported through it must not be a false positive even though esbuild
// cannot see the external's exports. ExportsComplete reports it incomplete.
func TestResolveNamedImports_ExternalExportStarNotFalsePositive(t *testing.T) {
	bodies := map[string]string{
		"barrel.js":  `export * from "three"; export const LOCAL = 1;`,
		"consume.js": `import { somethingFromThree } from "./barrel.js"; console.log(somethingFromThree);`,
	}
	if AnalyzeModules(bodies).ExportsComplete("barrel.js") {
		t.Fatalf("a barrel that stars from an external lib must be incomplete")
	}
	if issues := ResolveNamedImports("consume.js", bodies); len(issues) != 0 {
		t.Fatalf("a name re-exported from an external lib via export * must not be flagged, got %+v", issues)
	}
}

// Incompleteness propagates: barrelA stars from barrelB which stars from an
// external lib, so a name imported through barrelA is not a false positive.
func TestResolveNamedImports_TransitiveExternalStarNotFalsePositive(t *testing.T) {
	bodies := map[string]string{
		"barrelB.js": `export * from "three";`,
		"barrelA.js": `export * from "./barrelB.js"; export const X = 1;`,
		"consume.js": `import { deep } from "./barrelA.js"; console.log(deep);`,
	}
	f := AnalyzeModules(bodies)
	if f.ExportsComplete("barrelA.js") {
		t.Fatalf("barrelA stars (transitively) from an external lib and must be incomplete")
	}
	if issues := ResolveNamedImports("consume.js", bodies); len(issues) != 0 {
		t.Fatalf("a name behind a transitive external star must not be flagged, got %+v", issues)
	}
}

// A syntactically broken module does not produce phantom missing-export findings
// against its importers (the syntax gate owns that file); Missing suppresses them.
func TestAnalyzeModules_BrokenTargetSuppressed(t *testing.T) {
	bodies := map[string]string{
		"broken.js":  `export const a = (((;`, // does not parse
		"consume.js": `import { whatever } from "./broken.js"; console.log(whatever);`,
	}
	f := AnalyzeModules(bodies)
	if !f.broken["broken.js"] {
		t.Fatalf("broken.js should be recorded as broken")
	}
	if f.Missing("consume.js", "broken.js", "whatever") {
		t.Fatalf("missing-export against a broken target must be suppressed")
	}
	if issues := ResolveNamedImports("consume.js", bodies); len(issues) != 0 {
		t.Fatalf("no missing-import finding should pile onto a broken target, got %+v", issues)
	}
}

// factsFromEdges builds a ModuleFacts directly from "from -> to" pairs, so the
// cycle finder is tested as a pure graph algorithm without esbuild quirks.
func factsFromEdges(pairs ...[2]string) *ModuleFacts {
	f := &ModuleFacts{Edges: map[string][]GraphEdge{}}
	for _, p := range pairs {
		f.Edges[p[0]] = append(f.Edges[p[0]], GraphEdge{To: p[1]})
	}
	return f
}

// An acyclic graph (a diamond: a->b, a->c, b->d, c->d) has no cycle from any node.
func TestFindCycle_AcyclicGraph(t *testing.T) {
	f := factsFromEdges([2]string{"a", "b"}, [2]string{"a", "c"}, [2]string{"b", "d"}, [2]string{"c", "d"})
	if c := f.FindCycle(); c != nil {
		t.Fatalf("acyclic graph must report no cycle, got %v", c)
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		if c := f.CycleThrough(n); c != nil {
			t.Fatalf("CycleThrough(%q) must be nil in an acyclic graph, got %v", n, c)
		}
	}
}

// A self-edge is a one-node cycle.
func TestFindCycle_SelfLoop(t *testing.T) {
	f := factsFromEdges([2]string{"a", "a"})
	if got, want := f.CycleThrough("a"), []string{"a", "a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CycleThrough(a) = %v, want %v", got, want)
	}
}

// A two-node cycle is named from the lexicographically-smallest member, so the
// reported loop (and the fix-loop fingerprint) is stable across runs.
func TestFindCycle_TwoNodeCycleDeterministic(t *testing.T) {
	f := factsFromEdges([2]string{"b", "a"}, [2]string{"a", "b"})
	if got, want := f.FindCycle(), []string{"a", "b", "a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FindCycle = %v, want %v (smallest-node start)", got, want)
	}
	if got, want := f.CycleThrough("b"), []string{"b", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CycleThrough(b) = %v, want %v", got, want)
	}
}

// A three-node cycle reads as the full loop a -> b -> c -> a.
func TestFindCycle_ThreeNodeCycle(t *testing.T) {
	f := factsFromEdges([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"c", "a"})
	if got, want := f.FindCycle(), []string{"a", "b", "c", "a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FindCycle = %v, want %v", got, want)
	}
}

// Realistic entry<->leaf cycle through AnalyzeModules: the entry imports the leaf
// and the leaf imports a service back from the entry (a common back-edge in
// multi-module games). Both edges are static; the graph is cyclic and FindCycle names it.
func TestFindCycle_EntryLeafBackEdgeStatic(t *testing.T) {
	bodies := map[string]string{
		"game.js":      `import './game.tool.js'; export function register(s){} export function getScene(){ return {}; }`,
		"game.tool.js": `import { register, getScene } from './game.js'; export function beginNewGame(){ register('tool'); return getScene(); }`,
	}
	f := AnalyzeModules(bodies)
	if got, want := f.FindCycle(), []string{"game.js", "game.tool.js", "game.js"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FindCycle = %v, want %v", got, want)
	}
}

// A cycle that closes through a string-literal dynamic import() is still a cycle
// (Reaches and the finder both count dynamic edges): the entry pulls the leaf in
// via import(), and the leaf statically imports the entry back. Counting the dynamic
// edge is deliberate - it keeps the entry->leaf->entry coupling the client-JS
// architecture forbids visible even when the entry lazy-imports the leaf.
func TestFindCycle_ClosesThroughDynamicImport(t *testing.T) {
	bodies := map[string]string{
		"entry.js": `export function reg(){} import('./leaf.js');`,
		"leaf.js":  `import { reg } from './entry.js'; export function go(){ reg(); }`,
	}
	f := AnalyzeModules(bodies)
	if c := f.FindCycle(); c == nil {
		t.Fatalf("a cycle that closes through a dynamic import must be detected, got nil; edges=%v", edgeStrings(f))
	}
}

// Pins esbuild's link-error wording the analyzer parses. If an esbuild upgrade
// changes this message, this test fails loudly rather than the analyzer silently
// going blind to every missing export.
func TestNoMatchingExportRe_PinsEsbuildWording(t *testing.T) {
	bodies := map[string]string{
		"a.js":       `export const x = 1;`,
		"consume.js": `import { y } from "./a.js"; console.log(y);`,
	}
	f := AnalyzeModules(bodies)
	if !f.Missing("consume.js", "a.js", "y") {
		t.Fatalf("expected a parsed no-matching-export for y from a.js (esbuild wording may have changed); facts=%+v", f.missing)
	}
}
