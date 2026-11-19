package engine

// Module-role classification: the ONE home for deciding a build task's module
// roles (entry, zero-import store, leaf, page route) from its declared paths and
// edits. The coherence guard (plan_coherence.go), the deterministic normalizer
// (plan_normalize.go), the checklist renderer (build_seed.go), and the client-split
// guide selector (guide_prefetch.go, via fanoutEntry) all classify through these,
// so a convention change lives in one file instead of drifting across five.
//
// The path namespace is set by normPlanPath; every other predicate builds on it.
// Two deliberate divergences are kept, not merged: isStoreModuleName matches the
// three store suffixes (.state/.store/.core) while isZeroImportStateLeaf keys only
// on .state.js (a task shape, not a naming test).

import (
	"sort"
	"strings"
)

// normPlanPath normalises a planner-declared path for overlap comparison (case, leading
// slash, /files/ prefix) so "/files/list.js", "files/list.js", and "list.js" compare equal.
// A page route and a resource/table name are DIFFERENT namespaces ("/activity" the page vs
// "activity" the table), and folding them would merge a page task into a backend one and
// drop the page. So a root-absolute non-/files/ path keeps its slash; only file assets fold.
func normPlanPath(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	// The shared layout's canonical path is `default`; the plan frame tells the planner
	// `/layout` normalizes to it, so it must never read as a page route here (the
	// app-shape route-sprawl check would count it).
	if p == "/layout" {
		return "default"
	}
	if isRootedNonAsset(p) {
		return p // page-route namespace: keep the slash so it can't alias a bare name
	}
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimPrefix(p, "files/")
	return p
}

// isRootedNonAsset is the shared core of the page-route checks: a path that is
// rooted ("/...") and not a /files/ asset. Callers that also count the shared
// layout key "default" as a page OR it in explicitly. Distinct from isPageRoute
// (which also excludes /api/ and requires no file extension) and isPageRoutePath
// (which normalizes first).
func isRootedNonAsset(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "/files/")
}

// isClientModulePath reports whether a planner path is a client JS module
// (a /files/*.js or *.mjs asset), reusing normPlanPath's /files/ + case folding.
func isClientModulePath(p string) bool {
	n := normPlanPath(p)
	return strings.HasSuffix(n, ".js") || strings.HasSuffix(n, ".mjs")
}

// isPageRoutePath reports whether a planner path is a page route (a root-absolute
// path that is NOT a /files/ asset) rather than a client module or a bare table/API
// resource name. normPlanPath keeps page routes in the slash namespace, so a
// normalized path that still starts with "/" is a route.
func isPageRoutePath(p string) bool {
	return strings.HasPrefix(normPlanPath(p), "/")
}

// storeModuleSuffixes name the zero-import store module by convention
// (`app.state.js`, `app.store.js`, `app.core.js`).
var storeModuleSuffixes = []string{".state", ".store", ".core"}

// isStoreModuleName reports whether a client module path is named as the page's
// zero-import store module.
func isStoreModuleName(p string) bool {
	base := strings.ToLower(strings.TrimSpace(p))
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".js"), ".mjs")
	for _, s := range storeModuleSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

// stateLeafPath derives the conventional zero-import store module path from the
// entry module path: /files/game.js -> /files/game.state.js. A path already named as
// a store is returned unchanged, never doubled into game.state.state.js.
func stateLeafPath(entry string) string {
	e := strings.TrimSpace(entry)
	if isStoreModuleName(e) {
		return e
	}
	if strings.HasSuffix(strings.ToLower(e), ".js") {
		return e[:len(e)-len(".js")] + ".state.js"
	}
	return e + ".state.js"
}

// taskEditsModule reports whether the task's edits include the given normalized
// module path.
func taskEditsModule(t PlanTaskSpec, normModule string) bool {
	for _, e := range t.Edits {
		if normPlanPath(e) == normModule {
			return true
		}
	}
	return false
}

// taskOwnsClientModule reports whether the task owns (in its paths) any client JS
// module of its own.
func taskOwnsClientModule(t PlanTaskSpec) bool {
	for _, pth := range t.Paths {
		if isClientModulePath(pth) {
			return true
		}
	}
	return false
}

// taskOwnsPageRoute reports whether the task OWNs a page route - the mark of a
// spine/entry task, as opposed to a leaf or a data/endpoint task.
func taskOwnsPageRoute(t PlanTaskSpec) bool {
	for _, pth := range t.Paths {
		if isPageRoutePath(pth) {
			return true
		}
	}
	return false
}

// isZeroImportStateLeaf reports whether a task is the split-out store task: it owns
// a `*.state.js` client module, modifies no other task's file (no Edits), and owns
// no page route. Its file merely existing is all its completion needs, so it depends
// on nothing. Keyed on `.state.js` specifically (a task shape), not isStoreModuleName.
func isZeroImportStateLeaf(t PlanTaskSpec) bool {
	if len(t.Edits) > 0 || len(t.Paths) == 0 {
		return false
	}
	hasStateLeaf := false
	for _, p := range t.Paths {
		if isPageRoutePath(p) {
			return false // owns a route -> a spine/page task, not a store task
		}
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(p)), ".state.js") {
			hasStateLeaf = true
		}
	}
	return hasStateLeaf
}

// anyTaskEditsModuleExcept reports whether any task other than exceptIdx edits the
// normalized module path. Shared by the entry/store derivation below so "is this
// module wired into by a sibling" is answered one way.
func anyTaskEditsModuleExcept(p *Plan, exceptIdx int, norm string) bool {
	for j := range p.Tasks {
		if j != exceptIdx && taskEditsModule(p.Tasks[j], norm) {
			return true
		}
	}
	return false
}

// planModuleClaimed reports whether any task OWNs (paths) or EDITS the client module
// at normalized path norm - i.e. the path is already spoken for, so a normalization
// must not add it to a different task.
func planModuleClaimed(p *Plan, norm string) bool {
	for _, t := range p.Tasks {
		for _, pth := range t.Paths {
			if normPlanPath(pth) == norm {
				return true
			}
		}
		if taskEditsModule(t, norm) {
			return true
		}
	}
	return false
}

// leafEditedEntry returns the client module task i OWNs that ANOTHER task EDITS -
// the entry that leaves wire INTO - as (normalized, display) paths, or "","" when
// the task owns no such module. Picking the leaf-edited module (NOT just the last
// client path) means a task owning BOTH the entry and a zero-import store (app.js
// AND app.state.js) is judged by its entry: the store, which no leaf edits, is
// never mistaken for it. A store-NAMED module is never the entry even when leaves
// edit it (they add their exports to the shared store): taking it for the entry
// derived a second store `app.state.state.js` beside the real one.
func leafEditedEntry(p *Plan, i int) (norm, display string) {
	for _, pth := range p.Tasks[i].Paths {
		if !isClientModulePath(pth) || isStoreModuleName(pth) {
			continue
		}
		n := normPlanPath(pth)
		if anyTaskEditsModuleExcept(p, i, n) {
			return n, strings.TrimSpace(pth)
		}
	}
	return "", ""
}

// spineEntryModule returns the entry module of spine task i: the leaf-edited module
// when leaves wire in by editing it, else - when leaves wire in through deps and
// imports alone, so nothing edits the entry - the task's one non-store client module,
// provided the page is split (another task owns a client module). "","" when neither
// holds. Shared by entryExportsProblem and normalizeEntryExports so the guard and the
// normalizer identify the entry identically; before the deps-wired case was covered,
// a spine exporting from its entry passed both unflagged and the checklist told every
// leaf to import from the entry, which the save gate then rejected.
func spineEntryModule(p *Plan, i int) (norm, display string) {
	if n, d := leafEditedEntry(p, i); n != "" {
		return n, d
	}
	var only string
	for _, pth := range p.Tasks[i].Paths {
		if !isClientModulePath(pth) || isStoreModuleName(pth) {
			continue
		}
		if only != "" {
			return "", "" // several candidates: not a shape to guess through
		}
		only = pth
	}
	if only == "" {
		return "", ""
	}
	for j := range p.Tasks {
		if j != i && taskOwnsClientModule(p.Tasks[j]) {
			return normPlanPath(only), strings.TrimSpace(only)
		}
	}
	return "", ""
}

// taskExportHome returns the normalized path of task i's owned client module
// that no OTHER task edits (its zero-import store), excluding the entry, or ""
// when none exists. This is where the task's `exports` home: exports never
// live in the entry file, and a task owning entry + store is judged valid on
// that basis (the checklist scaffold points siblings at the store too).
func taskExportHome(p *Plan, i int, entryModule string) string {
	t := p.Tasks[i]
	for _, pth := range t.Paths {
		if !isClientModulePath(pth) {
			continue
		}
		norm := normPlanPath(pth)
		if norm == entryModule {
			continue
		}
		if !anyTaskEditsModuleExcept(p, i, norm) {
			return norm
		}
	}
	return ""
}

// fanoutEntry finds a shared ENTRY (a client module or page route OWNED by some
// task) that at least `min` tasks EDIT while owning no client module themselves.
// Deterministic, and shared by the coherence guard and the client-split guide
// selector so both treat route and .js-entry funnels alike.
func fanoutEntry(p *Plan, min int) (entry string, n int, ok bool) {
	if p == nil {
		return "", 0, false
	}
	owned := map[string]string{} // normalized -> display path
	for _, t := range p.Tasks {
		for _, pth := range t.Paths {
			if isClientModulePath(pth) || isPageRoutePath(pth) {
				owned[normPlanPath(pth)] = strings.TrimSpace(pth)
			}
		}
	}
	norms := make([]string, 0, len(owned))
	for k := range owned {
		norms = append(norms, k)
	}
	sort.Strings(norms) // deterministic: same plan -> same entry chosen
	for _, e := range norms {
		c := 0
		for _, t := range p.Tasks {
			if taskEditsModule(t, e) && !taskOwnsClientModule(t) {
				c++
			}
		}
		if c >= min {
			return owned[e], c, true
		}
	}
	return "", 0, false
}
