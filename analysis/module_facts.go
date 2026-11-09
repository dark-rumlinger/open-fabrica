package analysis

// esbuild-backed module analysis: one in-memory bundle over project JS bodies
// yields the resolved import graph and the named imports matching no export,
// decided by a real linker. Resolution goes through the SAME path logic as the
// renderer, so specifiers resolve to the exact path the browser requests.
// External and not-yet-saved specifiers are marked external so the bundle never
// hard-errors and the leaves-first build order stays unblocked.

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// moduleFactsNamespace is the esbuild namespace every in-memory project module is
// loaded under, so resolved module identities read as "ofvfs:js/world.js" and are
// trivially distinguished from on-disk paths (there are none).
const moduleFactsNamespace = "ofvfs"

// GraphEdge is one import edge from a module to a project module it imports. To
// is the resolved project path (always a member of the analyzed set); Dynamic is
// true for a dynamic `import()` with a string-literal argument.
type GraphEdge struct {
	To      string
	Dynamic bool
}

// ModuleFacts is the esbuild-derived analysis of a set of JS module bodies.
type ModuleFacts struct {
	// Edges maps an importer project path to its deduped import edges. Targets
	// are present project modules only; external libraries and not-yet-saved
	// siblings are not edges (they are not members of the graph).
	Edges map[string][]GraphEdge
	// missing holds missingKey(importer, target, name) for every named import the
	// linker reported as matching no export of its target.
	missing map[string]bool
	// broken is the set of input paths that failed to parse. The linker cannot
	// compute their exports, so a "no matching export" naming a broken target is
	// noise and is suppressed by Missing.
	broken map[string]bool
	// incompleteExports is the set of modules whose export surface is not
	// statically knowable (they transitively `export *` from an opaque source).
	// A missing-name import against such a target must not be flagged.
	incompleteExports map[string]bool
	// hardErrors holds, per module path, the bundler's binding/link errors that are
	// NOT missing-export (`missing` owns those) and NOT plain parse errors (`broken`
	// owns those): const assignment, import assignment, duplicate declarations.
	// Each entry is a short "message (line:col)" string, the JSSyntax shape.
	hardErrors map[string][]string
}

// noMatchingExportRe extracts the target module and imported name from esbuild's
// stable link-error text. Parsing the message is the one string coupling here;
// modulefacts_test.go pins it so an esbuild wording change fails loudly.
var noMatchingExportRe = regexp.MustCompile(`No matching export in "([^"]*)" for import "([^"]*)"`)

// exportStarFromRe captures the specifier of a wildcard re-export. Only
// `export *` can inject names a per-module analysis cannot enumerate; run on
// comment-stripped code so a commented-out star never counts.
var exportStarFromRe = regexp.MustCompile(`export\s*\*\s*(?:as\s+[A-Za-z_$][\w$]*\s+)?from\s*['"]([^'"]+)['"]`)

func missingKey(importer, target, name string) string {
	return importer + "\x00" + target + "\x00" + name
}

// Reaches reports whether `from` imports `target` directly or transitively
// (static and string-literal dynamic edges; an opaque dynamic import is not an
// edge). A module reaches itself.
func (f *ModuleFacts) Reaches(from, target string) bool {
	if f == nil {
		return false
	}
	if from == target {
		return true
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, e := range f.Edges[n] {
			if e.To == target {
				return true
			}
			if !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	return false
}

// Leaf reports whether module imports no other PROJECT module (a library import
// is not a graph edge) - the zero-import shared-leaf shape, a safe destination to
// suggest moving a misplaced shared symbol into without forming a cycle.
func (f *ModuleFacts) Leaf(module string) bool {
	return f == nil || len(f.Edges[module]) == 0
}

// FindCycle returns any import cycle as a node sequence beginning and ending on
// the same module, or nil when acyclic. Nodes and edges are visited in sorted
// order so the reported loop is stable across runs (the fix-loop fingerprints on it).
func (f *ModuleFacts) FindCycle() []string {
	if f == nil {
		return nil
	}
	nodes := make([]string, 0, len(f.Edges))
	for n := range f.Edges {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		if c := f.CycleThrough(n); c != nil {
			return c
		}
	}
	return nil
}

// CycleThrough returns a cycle that contains node - the sequence
// node -> … -> node - or nil when node is in no import cycle. The write gate uses
// it to reject the edit that closes a loop and to name it for the fix.
//
// Dynamic-import edges DO count, on purpose. A leaf importing a shared symbol back
// from the entry is the coupling the client-JS architecture steers away from even
// when the entry pulls that leaf in via a lazy `import()`; treating the dynamic edge
// as a cycle is what keeps the "move the symbol into the zero-import store module" steering
// firing in that common shape. A genuine lazy-load (a leaf that imports only from
// OTHER leaves, never the entry) forms no back-edge and so is never flagged here.
func (f *ModuleFacts) CycleThrough(node string) []string {
	if f == nil {
		return nil
	}
	succ := append([]GraphEdge(nil), f.Edges[node]...)
	sort.Slice(succ, func(i, j int) bool { return succ[i].To < succ[j].To })
	for _, e := range succ {
		if back := f.shortestPath(e.To, node); back != nil {
			// back is e.To … node; prepend the start so the loop reads
			// node -> e.To -> … -> node.
			return append([]string{node}, back...)
		}
	}
	return nil
}

// shortestPath returns the node sequence from `from` to `target` (inclusive), or
// nil when unreachable. BFS with sorted neighbors so the path is deterministic;
// from==target returns just [target].
func (f *ModuleFacts) shortestPath(from, target string) []string {
	if from == target {
		return []string{target}
	}
	prev := map[string]string{}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		edges := append([]GraphEdge(nil), f.Edges[n]...)
		sort.Slice(edges, func(i, j int) bool { return edges[i].To < edges[j].To })
		for _, e := range edges {
			if seen[e.To] {
				continue
			}
			seen[e.To] = true
			prev[e.To] = n
			if e.To == target {
				chain := []string{}
				for at := target; ; at = prev[at] {
					chain = append([]string{at}, chain...)
					if at == from {
						return chain
					}
				}
			}
			queue = append(queue, e.To)
		}
	}
	return nil
}

// ExportsComplete reports whether module's full named-export surface is
// statically knowable: false when it transitively `export *`s from an opaque
// source, where flagging a missing-name import would risk a false positive.
func (f *ModuleFacts) ExportsComplete(module string) bool {
	if f == nil {
		return true
	}
	return !f.incompleteExports[module]
}

// Missing reports whether the linker proved `name`, imported by `importer` from
// `target`, matches no export of `target`. False when either module failed to
// parse, so a spurious finding never piles onto a file already flagged for syntax.
func (f *ModuleFacts) Missing(importer, target, name string) bool {
	if f == nil || f.broken[importer] || f.broken[target] {
		return false
	}
	return f.missing[missingKey(importer, target, name)]
}

// HardErrors returns the binding/link errors esbuild reported against module -
// defects a single-file parse cannot see. nil when the module bundled clean or
// failed to parse (broken modules are the syntax gate's; avoids double-reporting).
func (f *ModuleFacts) HardErrors(module string) []string {
	if f == nil || f.broken[module] {
		return nil
	}
	return f.hardErrors[module]
}

// formatBuildError renders an esbuild bundler error as the same compact
// "message (line:col)" string JSSyntax produces for parse errors, so both write-
// gate channels speak one format to the model.
func formatBuildError(e api.Message) string {
	if e.Location != nil {
		return fmt.Sprintf("%s (%d:%d)", e.Text, e.Location.Line, e.Location.Column)
	}
	return e.Text
}

// AnalyzeModules bundles bodies (project path -> JS source) in memory with
// esbuild and returns the resolved import graph plus named imports matching no
// export. No disk or network I/O; pure and deterministic for a given input.
//
// The Edges are the RESOLUTION-CORRECTNESS graph: a broken module contributes no
// outgoing edges, so this graph must NOT be used for reachability (a broken entry
// would make its whole subtree look orphaned) - BuildModuleGraph owns that.
func AnalyzeModules(bodies map[string]string) *ModuleFacts {
	f := &ModuleFacts{Edges: map[string][]GraphEdge{}, missing: map[string]bool{}, broken: map[string]bool{}}
	if len(bodies) == 0 {
		return f
	}

	// A body that does not parse poisons the linker's view of its exports; record
	// it so Missing can suppress findings that name it, and so a parse error in
	// one module never manifests as a phantom missing-export in another.
	for p, b := range bodies {
		if r := JSSyntax(p, b); r.Gated && !r.Clean {
			f.broken[p] = true
		}
	}

	entries := make([]string, 0, len(bodies))
	for p := range bodies {
		entries = append(entries, p)
	}
	byName, _ := ProjectModuleMap(entries)
	// esbuild runs onResolve callbacks concurrently across parse goroutines, so
	// the shared edge bookkeeping is mutex-guarded.
	var mu sync.Mutex
	seenEdge := map[string]bool{}

	plugin := api.Plugin{
		Name: "of-vfs",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
				// Entry points: identify each project module under our namespace.
				if a.Kind == api.ResolveEntryPoint {
					return api.OnResolveResult{Path: a.Path, Namespace: moduleFactsNamespace}, nil
				}
				target, present, internal := resolveImportTarget(a.Importer, a.Path, bodies, byName)
				if !internal {
					// External library: the importmap owns it, not the graph.
					return api.OnResolveResult{Path: a.Path, External: true}, nil
				}
				if !present {
					// Not-yet-saved sibling (normal leaves-first order). External so
					// the bundle does not error; the advisory target_absent is
					// surfaced by ResolveNamedImportsWith, not here.
					return api.OnResolveResult{Path: a.Path, External: true}, nil
				}
				edgeKey := a.Importer + "\x00" + target
				mu.Lock()
				if !seenEdge[edgeKey] {
					seenEdge[edgeKey] = true
					f.Edges[a.Importer] = append(f.Edges[a.Importer], GraphEdge{
						To: target, Dynamic: a.Kind == api.ResolveJSDynamicImport,
					})
				}
				mu.Unlock()
				return api.OnResolveResult{Path: target, Namespace: moduleFactsNamespace}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: moduleFactsNamespace}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				body := bodies[a.Path]
				return api.OnLoadResult{Contents: &body, Loader: moduleLoader(a.Path)}, nil
			})
		},
	}

	res := api.Build(api.BuildOptions{
		EntryPoints: entries,
		Bundle:      true,
		Write:       false,
		Splitting:   true,
		Outdir:      "out",
		Format:      api.FormatESModule,
		Platform:    api.PlatformBrowser,
		TreeShaking: api.TreeShakingFalse,
		LogLevel:    api.LogLevelSilent,
		LogLimit:    0,
		Plugins:     []api.Plugin{plugin},
	})

	seenHard := map[string]bool{}
	for _, e := range res.Errors {
		if e.Location == nil {
			continue
		}
		if m := noMatchingExportRe.FindStringSubmatch(e.Text); m != nil {
			importer := stripModuleNS(e.Location.File)
			target := stripModuleNS(m[1])
			f.missing[missingKey(importer, target, m[2])] = true
			continue
		}
		// Any other bundler error pinned to a project module is a hard binding-level
		// defect. Skip modules the syntax gate already flagged (no double-reporting);
		// esbuild can repeat one error per entry point reaching the module, so dedupe.
		file := stripModuleNS(e.Location.File)
		if _, ok := bodies[file]; !ok || f.broken[file] {
			continue
		}
		msg := formatBuildError(e)
		if seenHard[file+"\x00"+msg] {
			continue
		}
		seenHard[file+"\x00"+msg] = true
		if f.hardErrors == nil {
			f.hardErrors = map[string][]string{}
		}
		f.hardErrors[file] = append(f.hardErrors[file], msg)
	}

	f.computeExportCompleteness(bodies, byName)
	return f
}

// computeExportCompleteness fills f.incompleteExports: a module with an
// `export *` the analyzer cannot see through is locally incomplete, and
// incompleteness propagates backward along internal `export *` edges to a fixpoint.
func (f *ModuleFacts) computeExportCompleteness(bodies map[string]string, byName map[string]string) {
	starEdges := map[string][]string{}
	incomplete := map[string]bool{}
	for p, b := range bodies {
		for _, spec := range exportStarSpecifiers(b) {
			target, present, internal := resolveImportTarget(p, spec, bodies, byName)
			if internal && present && !f.broken[target] {
				starEdges[p] = append(starEdges[p], target)
			} else {
				incomplete[p] = true // external lib, not-yet-saved, or broken: opaque surface
			}
		}
	}
	// Propagate: p is incomplete if it stars from any incomplete module.
	for changed := true; changed; {
		changed = false
		for p, targets := range starEdges {
			if incomplete[p] {
				continue
			}
			for _, t := range targets {
				if incomplete[t] {
					incomplete[p] = true
					changed = true
					break
				}
			}
		}
	}
	f.incompleteExports = incomplete
}

// exportStarSpecifiers returns the specifiers of every wildcard re-export in a JS
// body (comment-stripped first so a commented-out star never counts).
func exportStarSpecifiers(body string) []string {
	code := StripJSComments(body)
	matches := exportStarFromRe.FindAllStringSubmatch(code, -1)
	if matches == nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// stripModuleNS removes the analyzer's esbuild namespace prefix ("ofvfs:") from a
// path esbuild reports in an error location or message, yielding the project path.
func stripModuleNS(p string) string {
	if i := len(moduleFactsNamespace) + 1; len(p) >= i && p[:i] == moduleFactsNamespace+":" {
		return p[i:]
	}
	return p
}

// moduleLoader picks the esbuild loader for a project JS module path. Everything
// the analyzer loads is the JS family (CSS and assets are never graph nodes).
func moduleLoader(p string) api.Loader {
	if path.Ext(p) == ".jsx" {
		return api.LoaderJSX
	}
	return api.LoaderJS
}
