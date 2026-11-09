package content

// Cross-module import/export write gate for manage_files: proves a JS body's named
// imports resolve against the project and that the write does not drop an export a saved
// sibling still imports, catching contract mismatches the runtime probe would otherwise
// surface stages later.
//
// The gate hinges on one asymmetry: an import from a sibling not saved yet is the normal
// leaves-first flow and becomes an advisory `pending_imports` note, while an import of a
// name from a sibling that exists but lacks it is refused like a syntax error.

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"openfabrica/analysis"
	"openfabrica/store"
	"openfabrica/tools"
)

// isJSModulePath reports whether path is a JS module the import graph traverses.
// CSS is excluded: only the JS family carries the import/export surface this gate checks.
func isJSModulePath(path string) bool {
	lp := strings.ToLower(path)
	return strings.HasSuffix(lp, ".js") || strings.HasSuffix(lp, ".mjs")
}

// projectJSBodies loads every live inline JS file artifact as project-path -> body,
// overlaying the in-flight (focusPath, focusBody) so resolution reflects what THIS write
// stores. Disk-stored artifacts are skipped (JS is always inline). The focus path is
// canonical (post requireKindPath), matching the stored artifact paths.
func projectJSBodies(ctx *tools.ToolContext, focusPath, focusBody string) map[string]string {
	bodies := map[string]string{}
	arts, _ := store.ArtifactList(ctx.DB, store.ArtifactListOpts{Kind: "file", IncludeBody: true})
	for _, a := range arts {
		if a.Storage == store.StorageInline && isJSModulePath(a.Path) {
			bodies[a.Path] = a.Body
		}
	}
	bodies[focusPath] = focusBody
	return bodies
}

// rejectIfUnresolvedImports gates a JS-artifact write against the project's cross-module
// export surface, returning a non-nil failed Result (caller leaves the stored artifact
// untouched) when the write dangles a named import, removes an export a saved sibling
// imports, or calls a name a saved sibling exports without importing it. Otherwise it
// returns nil and a `pending` list of imports whose target is not saved yet.
//
// Each check fires only when the OTHER module already exists (a not-yet-saved sibling is
// a pending note, never a reject) and subtracts breaks already in oldBody, so the normal
// leaves-first flow and in-progress repairs are never trapped.
func rejectIfUnresolvedImports(ctx *tools.ToolContext, kind, path, oldBody, newBody, attachTo string) (*tools.Result, []map[string]any) {
	if kind != "file" || !isJSModulePath(path) || newBody == oldBody {
		return nil, nil
	}
	bodies := projectJSBodies(ctx, path, newBody)
	// One esbuild analysis of the post-write module set, reused by every check below.
	facts := analysis.AnalyzeModules(bodies)

	// --- consumer side: this file's named imports vs the project's exports ---
	issues := analysis.ResolveNamedImportsWith(path, bodies, facts)
	if oldBody != "" {
		// Subtract issues already present against the prior body so a partial
		// repair (or an edit unrelated to the dangling import) is not trapped.
		overlay := withOverlay(bodies, path, oldBody)
		prior := indexIssues(analysis.ResolveNamedImportsWith(path, overlay, analysis.AnalyzeModules(overlay)))
		issues = filterNewIssues(issues, prior)
	}
	var pending []map[string]any
	for _, is := range issues {
		if is.Reason == "name_missing" {
			return unresolvedImportResult(path, is, bodies, facts), nil
		}
		pending = append(pending, map[string]any{
			"name":   is.Imported,
			"from":   is.Spec,
			"target": is.Target,
			"status": "target_not_saved_yet",
		})
	}

	// --- static side: a binding-level defect the single-file syntax parse misses
	// (assigning to a const or an import, a duplicate declaration). The bundler above
	// already computed it; surface it here instead of at the browser probe. Subtracts
	// errors already in oldBody so an in-progress repair is not trapped. ---
	if rej := staticErrorResult(path, oldBody, bodies, facts); rej != nil {
		return rej, nil
	}

	// --- producer side: did this write drop an export a sibling imports? ---
	if removed := analysis.RemovedExports(oldBody, newBody); len(removed) > 0 {
		if breaks := analysis.BrokenImportersAfterExportChange(path, removed, bodies); len(breaks) > 0 {
			return exportRemovedResult(path, breaks), nil
		}
	}

	// --- forgotten-reference side: a bare `name(...)` this module neither declares,
	// imports, nor shares with the JS/DOM globals - a `name is not defined` throw the
	// import gates and esbuild miss (no import statement exists) and the page-load probe
	// misses when the call sits in an event handler. analysis.UndefinedCallRefs reads the
	// original source and is biased toward zero false positives. A name already undefined
	// against oldBody is pre-existing and left to proceed. ---
	if rej := forgottenRefResult(path, oldBody, bodies, projectLibraryGlobals(ctx)); rej != nil {
		return rej, nil
	}

	// --- unbound member/assign side: the same defect through the two shapes a CALL scan
	// cannot see - `name.prop` and `name = ...`. A leaf using a bare `THREE` an entry
	// imported as `import * as THREE from 'three'` has no import edge, so every gate above
	// passes and the page-load probe only catches it if that code runs at boot. Blocks
	// ONLY when the project's declared libraries make the answer certain (see
	// unboundRefSeverity's tools-side twin), so a UMD global the platform cannot name
	// never false-positives. ---
	if rej := unboundRefResult(ctx, path, oldBody, bodies); rej != nil {
		return rej, nil
	}

	// --- dynamic-import side: a name destructured from a LITERAL dynamic import the
	// target never exports - undefined at runtime. esbuild's linker and the static import
	// gate do not see dynamic-import destructuring, so this is the only static catch. ---
	if rej := dynamicImportResult(path, oldBody, bodies, facts); rej != nil {
		return rej, nil
	}

	// --- entry side (proactive): the page entry is the graph's unimported root, so any
	// symbol it exports is unreachable and cycle-bait (a leaf importing one trips the
	// cycle gate below). Catching it when the entry is written relocates the shared
	// surface once, before the leaves import it. ---
	if rej := entryExportsRejection(ctx, path, attachTo, oldBody, bodies, facts); rej != nil {
		return rej, nil
	}

	// --- cycle side: a write that CLOSES an import cycle through this file. Every name
	// resolves, so the gates above pass, yet a leaf importing back from the entry, or two
	// leaves importing each other, makes the graph cyclic. A cyclic ES graph still boots
	// but its module-eval order is brittle, leaving wiring silently dead. ---
	if rej := cycleRejection(ctx, path, attachTo, oldBody, bodies, facts); rej != nil {
		return rej, nil
	}

	// --- dead-window-global side: a `window.__` global this write READS that nothing in
	// the project assigns. The bridge gate below needs the symbol on two sides, so a read
	// written nowhere slips past it. Checked first: when a name is both dead and shared,
	// "create this value" beats "relocate it". ---
	if rej := windowDeadGlobalRejection(ctx, path, oldBody, bodies, facts); rej != nil {
		return rej, nil
	}

	// --- window-global bridge side: two modules sharing a `window.__` global in place
	// of an import. No import edge exists, so every gate above passes - this is the only
	// static catch for the import-graph-dodging bridge the client_js_split guide forbids. ---
	if rej := windowBridgeRejection(path, oldBody, bodies); rej != nil {
		return rej, nil
	}
	return nil, pending
}

// steerCreateRejectToResave clarifies a cross-module gate rejection when the write was a
// CREATE: the rejected save persisted nothing, so a follow-up patch bounces off
// notFoundForModify. It reframes "retry" as a full-body re-save, closing the
// create-reject -> patch -> not-found detour. On an UPDATE the prior artifact survives, so
// nil and exists pass through untouched.
func steerCreateRejectToResave(rej *tools.Result, kind, path string, exists bool) *tools.Result {
	if rej == nil || exists {
		return rej
	}
	rej.Error += fmt.Sprintf(
		" This save was CREATING %s %q, so the rejection persisted nothing - the file does not exist yet, so \"retry\" means call manage_files action=\"save\" again with the full corrected body; patch only modifies an already-saved artifact.",
		kind, path)
	return rej
}

// steerEditRejectToResave appends, to a patch rejection, the decision rule for re-applying
// the fix without thrashing: patch again only against bytes the model holds exactly (a
// fresh get or an untruncated write echo), else re-get the region or re-save the whole
// corrected body (which carries the model's own text and cannot drift). Anchoring a
// follow-up patch against bytes the model does not hold - a ranged read, or a line a
// platform auto-wire rewrote - is what loops (patch_anchor_not_found). No-op on a nil
// rejection, so it composes at the edit reject sites without a guard.
func steerEditRejectToResave(rej *tools.Result) *tools.Result {
	if rej == nil {
		return rej
	}
	rej.Error += " To re-apply: patch again ONLY against bytes you hold exactly (a fresh get or an untruncated write echo); otherwise re-get the region first, or re-save the whole corrected body with manage_files action=\"save\" (it carries your own text and cannot drift)."
	return rej
}

// tryDedupeImports resolves THIS write's redundant duplicate imports - the same name
// imported from the same sibling more than once, which esbuild rejects as an
// "already declared" binding error though removing the copy changes no semantics.
// analysis.DedupeNamedImports removes ONLY exact duplicates (same specifier and alias), so
// a same-name-different-source conflict is left to the reject, and the caller re-runs the
// gates and accepts only when clean. Returns the deduped body, the removed-import labels,
// and ok=true, or ("", nil, false) to leave the reject standing.
func tryDedupeImports(newBody string) (string, []string, bool) {
	deduped, removed := analysis.DedupeNamedImports(newBody)
	if deduped == newBody || len(removed) == 0 {
		return "", nil, false
	}
	return deduped, removed, true
}

// staticErrorResult refuses a write whose body carries a binding-level error the bundler
// proves but the single-file syntax parse misses. Fires only on an error THIS edit
// introduced (one already in oldBody is an in-progress repair, left to proceed); returns
// nil when there is nothing new to flag.
func staticErrorResult(path, oldBody string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	errs := facts.HardErrors(path)
	if len(errs) == 0 {
		return nil
	}
	if oldBody != "" {
		overlay := withOverlay(bodies, path, oldBody)
		prior := map[string]bool{}
		for _, e := range analysis.AnalyzeModules(overlay).HardErrors(path) {
			prior[e] = true
		}
		var fresh []string
		for _, e := range errs {
			if !prior[e] {
				fresh = append(fresh, e)
			}
		}
		errs = fresh
		if len(errs) == 0 {
			return nil
		}
	}
	return tools.FailCode("static_error",
		"write rejected: %s has a binding error the bundler caught that a plain syntax parse does not: %s. The artifact was not modified. This is a hard defect (e.g. reassigning a `const`, assigning to an imported binding, or a duplicate declaration) that throws at runtime - fix the named line, then retry.",
		path, strings.Join(errs, "; "))
}

// freshForgottenRefs returns the undefined call references THIS edit introduces: names
// the module calls but neither declares, imports, nor shares with the JS/DOM globals
// allowlist, minus the globals the project's declared classic libraries name (a bare
// `confetti()` or `new QRCode()` is bound by its <script src>), minus any already
// undefined against oldBody. Shared by the reject and the auto-wire path so both act on
// the same set.
func freshForgottenRefs(path, oldBody string, bodies map[string]string, libGlobals map[string]bool) []analysis.UndefinedRef {
	var refs []analysis.UndefinedRef
	for _, r := range analysis.UndefinedCallRefs(path, bodies[path]) {
		if !libGlobals[r.Name] {
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 || oldBody == "" {
		return refs
	}
	prior := map[string]bool{}
	for _, r := range analysis.UndefinedCallRefs(path, oldBody) {
		prior[r.Name] = true
	}
	var fresh []analysis.UndefinedRef
	for _, r := range refs {
		if !prior[r.Name] {
			fresh = append(fresh, r)
		}
	}
	return fresh
}

// forgottenRefResult refuses a write that calls a name the module neither declares,
// imports, nor shares with the JS/DOM globals allowlist. Fires only on a reference THIS
// edit introduced; returns nil when there is nothing new to flag.
//
// A capitalized call name is by JS convention a constructor / ambient global - a browser
// API (`new CSSStyleSheet()`, `new URLPattern()`, a WebGPU/WebTransport ctor) or a library
// global - not a forgotten SAME-MODULE helper. The curated jsGlobals allowlist can never be
// complete, and unlike the member/assign twin this call-position gate has no library-exactness
// escape hatch, so hard-rejecting an unlisted capitalized name blocks valid modern JS. Demote
// those to the post-build undefined_reference nudge (non-blocking per engine/blocking.go) and
// hard-reject only the lowercase names - the genuine forgotten-helper catch this gate exists
// for (a bare `getEventPos()` a sibling privately defines). A capitalized name that IS a
// forgotten sibling class is still auto-wired upstream (tryAutoWireImports reads the full
// freshForgottenRefs set); only the un-wireable remainder reaches here and passes through.
func forgottenRefResult(path, oldBody string, bodies map[string]string, libGlobals map[string]bool) *tools.Result {
	refs := freshForgottenRefs(path, oldBody, bodies, libGlobals)
	if len(refs) == 0 {
		return nil
	}
	var local []analysis.UndefinedRef
	for _, r := range refs {
		if !startsUpper(r.Name) {
			local = append(local, r)
		}
	}
	if len(local) == 0 {
		return nil
	}
	return forgottenRefRejection(path, local, bodies)
}

// startsUpper reports whether name's first rune is an uppercase letter - the JS
// constructor/class naming convention. A name led by `_`, `$`, or a digit is not
// uppercase, so it stays on the hard-reject (probable-local-helper) path.
func startsUpper(name string) bool {
	if name == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

// unboundRefResult refuses a write that uses a name in MEMBER or ASSIGN position that no
// enclosing scope binds - `THREE.Vector3`, `net.send(...)`, `engine = ...`. The call-position
// check above is structurally blind to these (the identifier is never followed by `(`), so
// without this the defect only surfaces at the runtime probe, and only if that line runs at
// boot.
//
// Fires only on a reference THIS edit introduced, and only when projectGlobalsAreExact says
// no declared library could be supplying the name - the same deterministic-only rule the
// round-level validator uses (engine/validators/static_globals.go), so the two gates agree
// on when the answer is knowable. Returns nil when there is nothing certain to flag.
func unboundRefResult(ctx *tools.ToolContext, path, oldBody string, bodies map[string]string) *tools.Result {
	libs, ok := projectExternalLibs(ctx)
	if !ok {
		return nil // no authoritative plan; a library the platform cannot see might supply the name
	}
	libGlobals, exact := analysis.LibraryGlobals(libs)
	if !exact {
		return nil // some classic <script src> defines a global nobody named; cannot judge
	}
	refs := freshUnboundRefs(path, oldBody, bodies, libGlobals)
	if len(refs) == 0 {
		return nil
	}
	return unboundRefRejection(path, refs, bodies, libs)
}

// freshUnboundRefs returns the member/assign references THIS edit introduces, minus the
// globals declared libraries supply and minus any already unbound against oldBody (an
// in-progress repair must not be trapped). Mirrors freshForgottenRefs.
func freshUnboundRefs(path, oldBody string, bodies map[string]string, libGlobals map[string]bool) []analysis.UndefinedRef {
	keep := func(body string) []analysis.UndefinedRef {
		var out []analysis.UndefinedRef
		for _, r := range analysis.UndefinedRefs(path, body) {
			if r.Kind != analysis.RefMember && r.Kind != analysis.RefAssign {
				continue
			}
			if libGlobals[r.Name] {
				continue // a global a declared classic <script src> really does define
			}
			out = append(out, r)
		}
		return out
	}
	refs := keep(bodies[path])
	if len(refs) == 0 || oldBody == "" {
		return refs
	}
	prior := map[string]bool{}
	for _, r := range keep(oldBody) {
		prior[r.Name] = true
	}
	var fresh []analysis.UndefinedRef
	for _, r := range refs {
		if !prior[r.Name] {
			fresh = append(fresh, r)
		}
	}
	return fresh
}

// unboundRefRejection builds the message for member/assign unbound names. It prefers the
// library steers over the project-export steer: a sibling that already imports the
// binding is the strongest evidence (copy-paste line), a DECLARED module library whose
// specifier matches the name covers the first use (no sibling to copy yet), and only
// then does ExplainUndefinedRef's export lookup run - it cannot resolve a library
// binding and would otherwise say only "define it in this module".
func unboundRefRejection(path string, refs []analysis.UndefinedRef, bodies map[string]string, libs []analysis.ExternalLib) *tools.Result {
	exportIdx := analysis.ExportIndex(bodies)
	declIdx := analysis.DeclaredTopLevelNames(bodies)
	byName := analysis.ModuleSpecifierIndex(bodies)
	externals := analysis.ExternalBindingsElsewhere(bodies, path)
	var b strings.Builder
	fmt.Fprintf(&b, "write rejected: %s uses name(s) no enclosing scope binds - neither declared in this module, imported, nor a known browser/JS global, and no declared library supplies them. The browser throws `<name> is not defined` the instant that code path runs, and an interaction-only path (a resize handler, a tick callback) never runs under the page-load probe. The artifact was not modified.", path)
	for _, r := range refs {
		steer := analysis.ExplainUnboundExternalRef(r, externals)
		if steer == "" {
			steer = analysis.ExplainDeclaredExternalRef(r, libs)
		}
		if steer == "" {
			steer = analysis.ExplainUndefinedRef(path, r, exportIdx, declIdx, byName)
		}
		fmt.Fprintf(&b, "\n  - `%s` (line %d): %s", r.Name, r.Line, steer)
	}
	b.WriteString("\nFix each, then retry.")
	return tools.FailCode("undefined_reference", "%s", b.String())
}

// otherExporters returns every module in srcs other than self - the siblings that
// export a name the saved module reaches for but never imported.
func otherExporters(srcs []string, self string) []string {
	var out []string
	for _, s := range srcs {
		if s != self {
			out = append(out, s)
		}
	}
	return out
}

// tryAutoWireImports resolves THIS write's forgotten references by auto-inserting the
// missing sibling imports, so the model never hand-edits an import line: the new name is
// merged into an existing import from that owner (analysis.AddNamedImports) or prepended
// as a fresh line.
//
// It fires ONLY when unambiguous and safe: every fresh forgotten CALL is exported by
// exactly ONE other module (zero or two exporters is a real ambiguity/defect left to the
// reject), and the added imports introduce no cycle. A fresh MEMBER use (`hooks.onX = fn`,
// `net.send(...)`) is wired the same way when exactly one sibling exports the name, and
// otherwise left to the unbound-reference gate: a library global has no project exporter.
// Returns the rewritten body, the import lines added, and ok=true, or ("", nil, false) to
// leave the reject standing. Deterministic (sources and names sorted), which the replay
// cache depends on.
func tryAutoWireImports(ctx *tools.ToolContext, path, oldBody, newBody string) (string, []string, bool) {
	bodies := projectJSBodies(ctx, path, newBody)
	exportIdx := analysis.ExportIndex(bodies)
	refs := freshForgottenRefs(path, oldBody, bodies, projectLibraryGlobals(ctx))
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r.Name] = true
	}
	for _, r := range freshMemberRefs(path, oldBody, bodies) {
		if !seen[r.Name] && len(otherExporters(exportIdx[r.Name], path)) == 1 {
			seen[r.Name] = true
			refs = append(refs, r)
		}
	}
	if len(refs) == 0 {
		return "", nil, false
	}
	byName := analysis.ModuleSpecifierIndex(bodies)
	// Key each addition by the specifier its import line will carry: the file's EXISTING
	// specifier for an owner it already imports from (so the name merges into that line),
	// else the suggested fresh specifier.
	addBySpec := map[string]map[string]bool{}
	for _, r := range refs {
		owners := otherExporters(exportIdx[r.Name], path)
		if len(owners) != 1 {
			return "", nil, false // 0 = undefined everywhere; >1 = ambiguous
		}
		src := owners[0]
		spec := analysis.ImportedSpecFrom(path, src, bodies)
		if spec == "" {
			spec = analysis.SuggestImportSpecifier(path, src, byName)
		}
		if addBySpec[spec] == nil {
			addBySpec[spec] = map[string]bool{}
		}
		addBySpec[spec][r.Name] = true
	}
	specs := make([]string, 0, len(addBySpec))
	for s := range addBySpec {
		specs = append(specs, s)
	}
	sort.Strings(specs)
	additions := make(map[string][]string, len(addBySpec))
	var added []string
	for _, s := range specs {
		names := make([]string, 0, len(addBySpec[s]))
		for n := range addBySpec[s] {
			names = append(names, n)
		}
		sort.Strings(names)
		additions[s] = names
		added = append(added, fmt.Sprintf("import { %s } from '%s';", strings.Join(names, ", "), s))
	}
	wired := analysis.AddNamedImports(newBody, additions)
	if wired == newBody {
		return "", nil, false
	}
	if analysis.AnalyzeModules(withOverlay(bodies, path, wired)).CycleThrough(path) != nil {
		return "", nil, false
	}
	return wired, added, true
}

// freshMemberRefs returns the member-position references (`name.prop`) THIS edit
// introduces that no scope binds. Assign-position names (`name = ...`) are left out: an
// import binding cannot be assigned, so wiring one would trade the reject for another.
func freshMemberRefs(path, oldBody string, bodies map[string]string) []analysis.UndefinedRef {
	member := func(body string) []analysis.UndefinedRef {
		var out []analysis.UndefinedRef
		for _, r := range analysis.UndefinedRefs(path, body) {
			if r.Kind == analysis.RefMember {
				out = append(out, r)
			}
		}
		return out
	}
	refs := member(bodies[path])
	if len(refs) == 0 || oldBody == "" {
		return refs
	}
	prior := map[string]bool{}
	for _, r := range member(oldBody) {
		prior[r.Name] = true
	}
	var fresh []analysis.UndefinedRef
	for _, r := range refs {
		if !prior[r.Name] {
			fresh = append(fresh, r)
		}
	}
	return fresh
}

// tryRewireWrongSourceImports resolves THIS write's WRONG-SOURCE imports - a named import
// of a real symbol from the wrong sibling - by moving the import to the module that
// actually exports it. Like tryAutoWireImports it fires only when the name is exported by
// exactly ONE other module (not the file itself, not the wrong target) and re-pointing it
// introduces no cycle; aliased imports are left to the reject. The caller re-validates and
// accepts only when clean. Returns the rewritten body, the corrected import lines, and
// ok=true, or ("", nil, false) to leave the reject. Deterministic (the replay cache
// depends on it).
func tryRewireWrongSourceImports(ctx *tools.ToolContext, path, oldBody, newBody string) (string, []string, bool) {
	_ = oldBody
	bodies := projectJSBodies(ctx, path, newBody)
	issues := analysis.ResolveNamedImportsWith(path, bodies, analysis.AnalyzeModules(bodies))
	if len(issues) == 0 {
		return "", nil, false
	}
	exportIdx := analysis.ExportIndex(bodies)
	byName := analysis.ModuleSpecifierIndex(bodies)
	seen := map[string]bool{}
	var relocs []analysis.ImportRelocation
	var moved []string
	for _, is := range issues {
		// name_missing = the target module EXISTS but exports no such name (a real
		// wrong-source break); target_absent is the normal mid-split pending case.
		if is.Reason != "name_missing" || is.Local != is.Imported {
			continue // (aliased imports are moved by the model, not auto-corrected)
		}
		key := is.Imported + "\x00" + is.Spec
		if seen[key] {
			continue
		}
		// Exactly one OTHER module (not self, not the wrong target) exports the name.
		owner, n := "", 0
		for _, o := range exportIdx[is.Imported] {
			if o == path || o == is.Target {
				continue
			}
			owner, n = o, n+1
		}
		if n != 1 {
			continue // 0 = nobody else owns it (a real defect); >1 = ambiguous
		}
		seen[key] = true
		to := analysis.SuggestImportSpecifier(path, owner, byName)
		relocs = append(relocs, analysis.ImportRelocation{Imported: is.Imported, FromSpec: is.Spec, ToSpec: to})
		moved = append(moved, "import { "+is.Imported+" } from '"+to+"';")
	}
	if len(relocs) == 0 {
		return "", nil, false
	}
	sort.Slice(relocs, func(i, j int) bool { return relocs[i].Imported < relocs[j].Imported })
	sort.Strings(moved)
	rewritten := analysis.RelocateNamedImports(newBody, relocs)
	if rewritten == newBody {
		return "", nil, false
	}
	// Never introduce a cycle (a symbol whose true owner is an ancestor of this leaf
	// must be pushed DOWN into a zero-import store module, not pulled up - that reject stands).
	if analysis.AnalyzeModules(withOverlay(bodies, path, rewritten)).CycleThrough(path) != nil {
		return "", nil, false
	}
	return rewritten, moved, true
}

// forgottenRefRejection builds the reject, using analysis.ExplainUndefinedRef (shared with
// the post-build validator) so the same defect reads the same wherever it is caught. It is
// reached only for names the platform could NOT auto-wire: a name no sibling exports, or
// one two siblings export (ambiguous). Listing every name lets one fix-pass correct them
// all. Refs arrive from analysis.UndefinedCallRefs, whose Kind is always "".
func forgottenRefRejection(path string, refs []analysis.UndefinedRef, bodies map[string]string) *tools.Result {
	exportIdx := analysis.ExportIndex(bodies)
	declIdx := analysis.DeclaredTopLevelNames(bodies)
	byName := analysis.ModuleSpecifierIndex(bodies)
	var b strings.Builder
	fmt.Fprintf(&b, "write rejected: %s uses name(s) that are neither declared in this module, imported, nor a known browser/JS global - the browser throws `<name> is not defined` the instant that code path runs (a page-load probe misses it when the call sits in an event handler). The artifact was not modified.", path)
	for _, r := range refs {
		fmt.Fprintf(&b, "\n  - `%s` (line %d): %s", r.Name, r.Line, analysis.ExplainUndefinedRef(path, r, exportIdx, declIdx, byName))
	}
	b.WriteString("\nFix each, then retry.")
	return tools.FailCode("undefined_reference", "%s", b.String())
}

// cycleRejection refuses the write that introduces an import cycle through path. Fires
// only on the edit that CLOSES the loop (a cycle already in oldBody is an in-flight repair,
// left to proceed). The loop can close from either direction, with path a member of the
// resulting cycle either way.
func cycleRejection(ctx *tools.ToolContext, path, attachTo, oldBody string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	cyc := facts.CycleThrough(path)
	if cyc == nil {
		return nil
	}
	if oldBody != "" {
		overlay := withOverlay(bodies, path, oldBody)
		if analysis.AnalyzeModules(overlay).CycleThrough(path) != nil {
			return nil // pre-existing cycle, not introduced by this write
		}
	}
	entry := cycleEntry(ctx, path, attachTo, cyc)
	if entry == "" {
		return peerCycleResult(path, cyc, bodies, facts)
	}
	return cyclicImportResult(path, entry, cyc, bodies, facts)
}

// cycleEntry returns the cycle member that is a page entry (attached to a page, or being
// attached by this write), or "" when every member is a feature leaf. Only page
// attachment makes a module the entry: in-degree cannot tell, because in a two-leaf loop
// each side is imported by the other.
func cycleEntry(ctx *tools.ToolContext, path, attachTo string, cyc []string) string {
	for _, n := range cyc {
		if n == path && attachTo != "" {
			return n
		}
		if ctx != nil && ctx.DB != nil {
			if _, ok := store.PageForAsset(ctx.DB, n); ok {
				return n
			}
		}
	}
	return ""
}

// peerCycleResult is the reject for a loop between feature leaves, none of them the page
// entry. Every cycle the builds actually closed had this shape: two leaves that each call
// into the other. The fix is to turn the call that points back into a notification
// through the store (analysis.CyclePeerRemedy), not to move a leaf's API into the store.
func peerCycleResult(path string, cyc []string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	loop := make([]string, len(cyc))
	for i, n := range cyc {
		loop[i] = baseName(n)
	}
	msg := fmt.Sprintf(
		"write rejected: this import closes a cyclic module graph (%s). The artifact was not modified. None of these is the page entry: they are feature modules that each call into the other, and imports must run one way - a cycle boots with its module-eval order brittle and its wiring silently dead. %s",
		strings.Join(loop, " -> "), analysis.CyclePeerRemedy)
	if len(cyc) > 1 {
		if dest := cycleDest(path, cyc[1], bodies, facts); dest != "" {
			msg += fmt.Sprintf(" The zero-import store module both can import is `%s`.", baseName(dest))
		}
	}
	return tools.FailCode("cyclic_import", "%s", msg+" Then retry.")
}

// entryExportsRejection proactively refuses saving a PAGE-ENTRY module that declares
// exports. A page entry loads as the page's `<script type=module>` root and nothing
// imports it, so every exported symbol is unreachable and cycle-bait (a leaf importing one
// closes a leaf -> entry -> leaf cycle). Firing when the entry is written relocates the
// shared surface once, before the leaves import it.
//
// Scoped tight to stay false-positive-safe: fires only when path is a page entry, nothing
// imports it (in-degree 0 among saved modules), it imports >=1 project sibling (a real
// spine, not a standalone page), and it exports a symbol this edit ADDED.
func entryExportsRejection(ctx *tools.ToolContext, path, attachTo, oldBody string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	if ctx == nil {
		return nil
	}
	page := attachTo
	if page == "" && ctx.DB != nil {
		if p, ok := store.PageForAsset(ctx.DB, path); ok {
			page = p
		}
	}
	if page == "" {
		return nil // not a page entry: leaves are imported, never page-attached
	}
	// in-degree 0: no saved sibling imports this module. If one does, its exports
	// are legitimately consumed (e.g. a second page-attached module imports it).
	for _, edges := range facts.Edges {
		for _, e := range edges {
			if e.To == path {
				return nil
			}
		}
	}
	if len(facts.Edges[path]) == 0 {
		return nil // imports no project sibling: a standalone page, not a spine
	}
	surface := hubExportSurface(path, bodies)
	if len(surface) == 0 {
		return nil
	}
	// In-flight tolerance: only fire on an export this edit ADDED, so a mid-
	// relocation save that still carries the old surface is not re-trapped.
	if oldBody != "" {
		prior := map[string]bool{}
		for _, n := range hubExportSurface(path, withOverlay(bodies, path, oldBody)) {
			prior[n] = true
		}
		added := false
		for _, n := range surface {
			if !prior[n] {
				added = true
				break
			}
		}
		if !added {
			return nil
		}
	}

	msg := fmt.Sprintf(
		"write rejected: `%s` is a page entry module (attached to page %q, loaded as its `<script type=module>`), so it is the module graph's unimported root - nothing imports it. It declares %d export(s): %s. Those exports are unreachable from the entry, and the moment any leaf imports one it closes a cycle (leaf -> `%s` -> leaf) that boots with its wiring silently dead. The artifact was not modified.",
		baseName(path), page, len(surface), strings.Join(surface, ", "), baseName(path))

	if dest := cycleDest(path, path, bodies, facts); dest != "" {
		msg += fmt.Sprintf(
			" Home that surface in the zero-import store module `%s`: `export` it there, `import` it into `%s` from '%s', and have leaves import it from `%s` (not from `%s`). No leaf imports it yet, so nothing else needs to change this turn.",
			baseName(dest), baseName(path), analysis.SuggestImportSpecifier(path, dest, analysis.ModuleSpecifierIndex(bodies)), baseName(dest), baseName(path))
	} else {
		msg += fmt.Sprintf(
			" Home that surface in a zero-import store module (one that imports no other project module) and `export` it there - do not `export` from `%s`. The entry then imports it from that store like everything else. No leaf imports it yet, so nothing else needs to change this turn.",
			baseName(path))
	}
	return tools.FailCode("entry_exports", "%s", msg)
}

// cyclicImportResult builds the reject for a cycle through the page entry. Beyond naming
// the loop, it names the entry and its FULL exported surface, plus the zero-import store
// module to move that surface into, so the model relocates every shared symbol in ONE
// turn. The entry is the unimported root, so every name it exports is one a leaf will
// import back and close a cycle - the full surface IS the coupling surface.
func cyclicImportResult(path, entry string, cyc []string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	loop := make([]string, len(cyc))
	for i, n := range cyc {
		loop[i] = baseName(n)
	}
	preamble := fmt.Sprintf(
		"write rejected: this import closes a cyclic module graph (%s). The artifact was not modified. %s",
		strings.Join(loop, " -> "), analysis.CycleTreeNote)

	hub := entry
	surface := hubExportSurface(hub, bodies)
	if len(surface) == 0 {
		// No statically-knowable export surface for the hub (e.g. an opaque `export *`).
		// Fall back to the generic steer rather than name an empty list.
		return tools.FailCode("cyclic_import", "%s", preamble+" "+analysis.CycleLeafRemedy+" Then retry.")
	}

	symbols := fmt.Sprintf(
		" `%s` is the page entry (the unimported root) yet it exports %d symbol(s): %s. Nothing should import the entry, so every one of these couples a leaf back to it and closes a cycle - not just the one this edit reached for.",
		baseName(hub), len(surface), strings.Join(surface, ", "))

	var remedy string
	if dest := cycleDest(path, hub, bodies, facts); dest != "" {
		remedy = fmt.Sprintf(
			" Move them ALL, in ONE turn, into the zero-import store module `%s` and `export` them there; then `import` them into `%s` from '%s', and have every leaf import them from `%s` too. Then retry the leaves. Relocating them one at a time re-triggers this gate for every leaf that needs a different symbol.",
			baseName(dest), baseName(hub), analysis.SuggestImportSpecifier(hub, dest, analysis.ModuleSpecifierIndex(bodies)), baseName(dest))
	} else {
		remedy = fmt.Sprintf(
			" Move them ALL, in ONE turn, into a zero-import store module (one that imports no other project module) that every module imports, and `export` them there - do not `export` them from `%s`. Then retry the leaves. Relocating them one at a time re-triggers this gate for every leaf that needs a different symbol.",
			baseName(hub))
	}

	return tools.FailCode("cyclic_import", "%s", preamble+symbols+remedy)
}

// hubExportSurface returns hub's full named-export surface (sorted, deduped), read
// through the comment-safe ExportIndex so a commented-out `export` never registers.
func hubExportSurface(hub string, bodies map[string]string) []string {
	var out []string
	for name, srcs := range analysis.ExportIndex(bodies) {
		for _, s := range srcs {
			if s == hub {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// cycleDest picks an existing zero-import store module to move the shared symbols into,
// preferring one BOTH the hub and the stuck file already import (the relocation then
// adds no new edge on either side), then one either imports. Returns "" when neither
// imports one, and the caller tells the model to use or create one: any other zero-import
// file is a guess, and a guess named confidently (a login page's helper offered as "the
// store") steers the fix into an unrelated module. Within each tier a store-named module
// (`app.state.js`) wins over another zero-import module.
func cycleDest(path, hub string, bodies map[string]string, facts *analysis.ModuleFacts) string {
	imports := func(from, to string) bool {
		for _, e := range facts.Edges[from] {
			if e.To == to {
				return true
			}
		}
		return false
	}
	var leaves []string
	for m := range bodies {
		if m == path || m == hub {
			continue
		}
		if facts.Leaf(m) {
			leaves = append(leaves, m)
		}
	}
	sort.Slice(leaves, func(i, j int) bool {
		si, sj := storeNamed(leaves[i]), storeNamed(leaves[j])
		if si != sj {
			return si
		}
		return leaves[i] < leaves[j]
	})
	for _, m := range leaves {
		if imports(path, m) && imports(hub, m) {
			return m
		}
	}
	for _, m := range leaves {
		if imports(path, m) || imports(hub, m) {
			return m
		}
	}
	return ""
}

// storeNamed reports whether a module path is named as the page's zero-import store
// (`app.state.js`, `app.store.js`, `app.core.js`).
func storeNamed(p string) bool {
	base := strings.ToLower(p)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".js"), ".mjs")
	return strings.HasSuffix(base, ".state") || strings.HasSuffix(base, ".store") || strings.HasSuffix(base, ".core")
}

// dynamicImportResult rejects a write that destructures a name from a literal dynamic
// import the target does not export. Fires only when the target already exists with a
// knowable export surface, and subtracts misses already in oldBody so an in-progress
// repair is never trapped. Returns nil when there is nothing new to flag.
func dynamicImportResult(path, oldBody string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	missing := dynamicNameMissing(path, bodies, facts)
	if len(missing) == 0 {
		return nil
	}
	if oldBody != "" {
		overlay := withOverlay(bodies, path, oldBody)
		prior := map[string]bool{}
		for _, is := range dynamicNameMissing(path, overlay, analysis.AnalyzeModules(overlay)) {
			prior[is.Spec+"\x00"+is.Imported] = true
		}
		var fresh []analysis.ImportIssue
		for _, is := range missing {
			if !prior[is.Spec+"\x00"+is.Imported] {
				fresh = append(fresh, is)
			}
		}
		missing = fresh
		if len(missing) == 0 {
			return nil
		}
	}
	return dynamicUnresolvedResult(path, missing[0], bodies)
}

// dynamicNameMissing keeps only the hard "dynamic_name_missing" issues (the
// advisory "dynamic_target_absent", a not-yet-saved target, is never a reject).
func dynamicNameMissing(path string, bodies map[string]string, facts *analysis.ModuleFacts) []analysis.ImportIssue {
	var out []analysis.ImportIssue
	for _, is := range analysis.ResolveDynamicNamedImports(path, bodies, facts) {
		if is.Reason == "dynamic_name_missing" {
			out = append(out, is)
		}
	}
	return out
}

// dynamicUnresolvedResult builds the reject. Like unresolvedImportResult it prints the
// target's real export surface and, when the name lives in another project module, a
// cross-module "did you mean" steer.
func dynamicUnresolvedResult(path string, is analysis.ImportIssue, bodies map[string]string) *tools.Result {
	var surface string
	switch {
	case len(is.Exports) > 0 && is.HasDefault:
		surface = fmt.Sprintf(" %s exports %s as named exports, plus a default export.", baseName(is.Target), strings.Join(is.Exports, ", "))
	case len(is.Exports) > 0:
		surface = fmt.Sprintf(" %s exports %s (named).", baseName(is.Target), strings.Join(is.Exports, ", "))
	default:
		surface = fmt.Sprintf(" %s exports no named values.", baseName(is.Target))
	}
	var hint string
	for _, src := range analysis.ExportIndex(bodies)[is.Imported] {
		if src == is.Target || src == path {
			continue
		}
		hint = fmt.Sprintf(" `%s` is exported by `%s` - destructure it from there instead, or import it statically.", is.Imported, baseName(src))
		break
	}
	return tools.FailCode("dynamic_import_unresolved",
		"write rejected: %s dynamically imports { %s } from %q, but that module exports no name %q, so it is `undefined` at runtime (throws `%s is not a function` / a TypeError when used). The artifact was not modified.%s%s Fix the destructured name or the source module, then retry.",
		path, is.Imported, is.Spec, is.Imported, is.Imported, surface, hint)
}

// withOverlay returns a shallow copy of bodies with one path's body replaced, so
// the prior-state pass does not mutate the shared in-flight map.
func withOverlay(bodies map[string]string, path, body string) map[string]string {
	out := make(map[string]string, len(bodies))
	for k, v := range bodies {
		out[k] = v
	}
	out[path] = body
	return out
}

func issueKey(is analysis.ImportIssue) string {
	return is.Reason + "\x00" + is.Spec + "\x00" + is.Imported
}

func indexIssues(in []analysis.ImportIssue) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, is := range in {
		m[issueKey(is)] = true
	}
	return m
}

func filterNewIssues(in []analysis.ImportIssue, prior map[string]bool) []analysis.ImportIssue {
	var out []analysis.ImportIssue
	for _, is := range in {
		if !prior[issueKey(is)] {
			out = append(out, is)
		}
	}
	return out
}

// baseName returns the last path segment without pulling in the path package
// (whose name collides with the `path` argument threaded through this file).
func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// unresolvedImportResult builds the consumer-side reject. The actionable part is the
// target's real export surface, so the model corrects in one shot. When the name is
// exported by some other project module, a cross-module "did you mean" hint points there,
// turning the common wrong-sibling import into a one-shot fix.
func unresolvedImportResult(path string, is analysis.ImportIssue, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	var surface string
	switch {
	case len(is.Exports) > 0 && is.HasDefault:
		surface = fmt.Sprintf(" %s exports %s as named exports, plus a default export.", baseName(is.Target), strings.Join(is.Exports, ", "))
	case len(is.Exports) > 0:
		surface = fmt.Sprintf(" %s exports %s (named), and no default export.", baseName(is.Target), strings.Join(is.Exports, ", "))
	case is.HasDefault:
		surface = fmt.Sprintf(" %s has only a default export, no named exports - import it as `import %s from %q`.", baseName(is.Target), is.Local, is.Spec)
	default:
		surface = fmt.Sprintf(" %s exports nothing.", baseName(is.Target))
	}
	var hint string
	byName := analysis.ModuleSpecifierIndex(bodies)
	for _, src := range analysis.ExportIndex(bodies)[is.Imported] {
		if src == is.Target || src == path {
			continue // the target it (wrongly) imported from, or itself
		}
		// Cycle-aware: if `src` (the module that exports the name) imports THIS module
		// back, directly or transitively, then src is an ancestor of this leaf (typically
		// the entry). Pulling a shared symbol up from it re-couples the leaf to the entry
		// and risks a cyclic graph; the clean fix is to push the symbol DOWN into a
		// zero-import store module both can import.
		if facts.Reaches(src, path) {
			if facts.Leaf(is.Target) {
				hint = fmt.Sprintf(" `%s` lives in `%s`, but `%s` imports `%s` (it is the entry / an ancestor of this leaf), so importing `%s` from there couples this leaf back to the entry - the direction that risks a cyclic graph the page can't boot. Move `%s` into the zero-import store module `%s` you tried to import it from and `export` it there, then `import { %s } from '%s';`.",
					is.Imported, baseName(src), baseName(src), baseName(path), is.Imported, is.Imported, baseName(is.Target), is.Imported, analysis.SuggestImportSpecifier(path, is.Target, byName))
			} else {
				hint = fmt.Sprintf(" `%s` lives in `%s`, but `%s` imports `%s` (it is the entry / an ancestor of this leaf), so importing `%s` from there couples this leaf back to the entry - the direction that risks a cyclic graph the page can't boot. Move `%s` into a zero-import store module (one that imports no other project module) that both import, and `export` it there.",
					is.Imported, baseName(src), baseName(src), baseName(path), is.Imported, is.Imported)
			}
			break
		}
		hint = fmt.Sprintf(" `%s` is exported by `%s` - did you mean `import { %s } from '%s';`?", is.Imported, baseName(src), is.Imported, analysis.SuggestImportSpecifier(path, src, byName))
		break
	}
	return tools.FailCode("unresolved_import",
		"write rejected: %s imports { %s } from %q, but that module exports no name %q. The artifact was not modified.%s%s Fix the import or add the missing export to the target, then retry.",
		path, is.Imported, is.Spec, is.Imported, surface, hint)
}

// exportRemovedResult builds the producer-side reject, listing every saved
// importer broken by the dropped export so the model fixes the whole set in one
// turn rather than rediscovering them one at a time.
func exportRemovedResult(path string, breaks []analysis.ImporterBreak) *tools.Result {
	byName := map[string][]string{}
	var order []string
	for _, b := range breaks {
		if _, ok := byName[b.Imported]; !ok {
			order = append(order, b.Imported)
		}
		byName[b.Imported] = append(byName[b.Imported], b.Importer)
	}
	sort.Strings(order)
	var parts []string
	for _, name := range order {
		importers := byName[name]
		sort.Strings(importers)
		parts = append(parts, fmt.Sprintf("`%s` (imported by %s)", name, strings.Join(importers, ", ")))
	}
	return tools.FailCode("export_removed_in_use",
		"write rejected: this edit removes export(s) from %s that saved modules still import: %s. The artifact was not modified. Keep the export(s), or update those importers in the same turn.",
		path, strings.Join(parts, "; "))
}

// addImportStatus records cross-module resolution health on a successful write, so the
// model gets the same positive signal it gets for syntax without a separate check.
// pending_imports lists named imports whose target is not saved yet (expected mid-split);
// imports_resolved=true with no pending list means the graph is wired.
func addImportStatus(data map[string]any, pending []map[string]any) {
	if len(pending) == 0 {
		data["imports_resolved"] = true
		return
	}
	data["imports_resolved"] = false
	data["pending_imports"] = pending
}

// projectJSPaths lists every live inline JS module artifact's path. Paths only - the
// collision question is about names, and loading bodies for it would make a cheap
// check as expensive as the import-resolution pass.
func projectJSPaths(ctx *tools.ToolContext) []string {
	var out []string
	arts, _ := store.ArtifactList(ctx.DB, store.ArtifactListOpts{Kind: "file"})
	for _, a := range arts {
		if isJSModulePath(a.Path) {
			out = append(out, a.Path)
		}
	}
	return out
}

// rejectIfCollidingModuleName refuses a JS save whose basename another project module
// already answers to. Only a NEW file is gated: re-saving a file that is already in the
// collision would trap the very repair that resolves it, and the model's way out of an
// existing collision is a rename, which arrives on this path as a new name.
//
// The two unresolvable-specifier cases need OPPOSITE fixes - declare a library vs.
// rename one of your own files - so this message says which one this is, exactly as the
// post-build validator's Collides branch does.
func rejectIfCollidingModuleName(ctx *tools.ToolContext, path string, exists bool) *tools.Result {
	if exists {
		return nil
	}
	existing := projectJSPaths(ctx)
	name := analysis.CollidingModuleName(existing, path)
	if name == "" {
		return nil
	}
	others := analysis.CollidingModulePaths(existing, name)
	return tools.FailCode("colliding_module_name",
		"refusing to save %q: the module specifier %q is already taken by %s. Two modules sharing a basename make that bare name ambiguous, so it is dropped from the importmap and BOTH files stop resolving - the browser throws `Failed to resolve module specifier \"%s\"` at load. Do NOT declare it as a library. Save this one under a distinct basename (e.g. a `<app>.<group>.js` name that says which subsystem it is), or rename the existing file (manage_files action=rename) if that is the one whose name should change.",
		path, name, strings.Join(others, ", "), name,
	)
}
