package validators

// The static-code validator: the no-browser, pure-DB tier of post-build
// validation. Most checks reuse the EXACT primitives the save-time write gates
// use, so "valid on save" and "valid on build" are one body of code; a couple
// (arity_mismatch, module_specifier) are build-only. Every check is
// deterministic, so it can drive a fix-pass without looping. Per-check
// rationale lives at each check site in Validate below.

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/store"
)

type staticCodeValidator struct{}

func (staticCodeValidator) Name() string { return "static_code" }

// Validate parses every inline code artifact and resolves the JS import graph.
// baseURL/hostHeader are unused: this tier is pure static analysis over the
// project DB, mirroring the resolver the runtime server uses.
func (staticCodeValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	arts, err := store.ArtifactList(pdb, store.ArtifactListOpts{Kind: "file", IncludeBody: true})
	if err != nil {
		return nil
	}

	// One pass: collect inline file bodies and index JS module bodies, which the
	// import resolver needs as a whole set so a specifier can resolve its target.
	type fileBody struct{ path, body string }
	var files []fileBody
	jsBodies := map[string]string{}
	managed := map[string]bool{}
	for _, a := range arts {
		if a == nil || a.Storage != store.StorageInline {
			continue
		}
		if isJSModuleFile(a.Path) {
			jsBodies[a.Path] = a.Body
		}
		// A platform-emitted helper stays in the module set (siblings import from it) but
		// is never judged: a finding there would be one the model is told not to edit.
		if pm, _ := a.Metadata["platform_managed"].(bool); pm {
			managed[a.Path] = true
			continue
		}
		files = append(files, fileBody{a.Path, a.Body})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	var issues []Issue
	// jsBroken doubles as the skip set for every per-file check below.
	jsBroken := managed
	for _, f := range files {
		// js_syntax / css_syntax: the same gated parsers the write path uses.
		if r := analysis.JSSyntax(f.path, f.body); r.Gated && !r.Clean {
			jsBroken[f.path] = true
			issues = append(issues, Issue{
				Severity: store.SeverityError, Category: "js_syntax", File: f.path, Deterministic: true,
				Message: analysis.ExplainJSSyntaxError(f.path, r.Detail),
			})
			continue // a file that does not parse cannot have its imports judged
		}
		if r := analysis.CSSSyntax(f.path, f.body); r.Gated && !r.Clean {
			issues = append(issues, Issue{
				Severity: store.SeverityError, Category: "css_syntax", File: f.path, Deterministic: true,
				Message: fmt.Sprintf("`%s` has a CSS structure error: %s.", f.path, r.Detail),
			})
		}
	}

	// module_imports: one aggregated finding per JS file whose named imports do
	// not resolve. Sorted for stable output so the fix-pass seed is byte-stable.
	jsPaths := make([]string, 0, len(jsBodies))
	for p := range jsBodies {
		jsPaths = append(jsPaths, p)
	}
	sort.Strings(jsPaths)
	// One esbuild bundle over the whole module set, reused for every file's
	// import check below, instead of re-bundling per file.
	facts := analysis.AnalyzeModules(jsBodies)
	for _, p := range jsPaths {
		if jsBroken[p] {
			continue
		}
		probs := analysis.ResolveNamedImportsWith(p, jsBodies, facts)
		if len(probs) == 0 {
			continue
		}
		issues = append(issues, Issue{
			Severity: store.SeverityError, Category: "module_imports", File: p, Deterministic: true,
			Message: analysis.RenderImportProblems(p, probs),
		})
	}

	// undefined_reference: a bare `name(...)` a module neither declares, imports, nor
	// shares with the JS/DOM globals - the source-level backstop for the write gate's
	// forgotten-reference check, for a body that reached the DB through a path the
	// save gate did not fully cover. Deterministic: UndefinedCallRefs sorts by name,
	// and the index lookups are over sorted module sets. One finding per file.
	exportIdx := analysis.ExportIndex(jsBodies)
	declIdx := analysis.DeclaredTopLevelNames(jsBodies)
	specIdx := analysis.ModuleSpecifierIndex(jsBodies)
	// A global a declared classic <script src> names (`new QRCode()`, `confetti()`) is
	// bound at runtime: flagging it sent a build into a fix-pass that rewrote working
	// calls to window.QRCode. Same allowlist as the member/assign check below.
	unboundSev, unboundDet, libGlobals := unboundRefSeverity(pdb)
	for _, p := range jsPaths {
		if jsBroken[p] {
			continue
		}
		var refs []analysis.UndefinedRef
		for _, r := range analysis.UndefinedCallRefs(p, jsBodies[p]) {
			if !libGlobals[r.Name] {
				refs = append(refs, r)
			}
		}
		if len(refs) == 0 {
			continue
		}
		issues = append(issues, Issue{
			// Same severity rule as the member/assign check: a classic library that names
			// no global could be supplying the call, so the answer is not certain.
			Severity: unboundSev, Category: "undefined_reference", File: p, Deterministic: unboundDet,
			// Signature discriminates this check from the member/assign one below:
			// both emit category=undefined_reference with the same Page+File, so
			// without it one file's two findings share a Fingerprint and triageRound
			// would demote one on the other's fix-pass.
			Signature: "call",
			Message:   renderUndefinedRefs(p, refs, exportIdx, declIdx, specIdx),
		})
	}

	// undefined_reference (member and assignment positions): the same defect the
	// call check above reports, through the two shapes a call scan cannot see -
	// `name.prop` and `name = ...`. Resolution is scope-aware. Severity is
	// per-project (unboundRefSeverity, see static_globals.go): error+deterministic
	// when no declared library could supply the name as an ambient global, else a
	// warning. One finding per file.
	for _, p := range jsPaths {
		if jsBroken[p] {
			continue
		}
		var unbound []analysis.UndefinedRef
		for _, r := range analysis.UndefinedRefs(p, jsBodies[p]) {
			if r.Kind != analysis.RefMember && r.Kind != analysis.RefAssign {
				continue
			}
			// A global a declared classic <script src> defines is genuinely bound at
			// runtime, so it is never a finding - that allowlist is exactly what makes
			// the rest of this check safe to gate at error.
			if libGlobals[r.Name] {
				continue
			}
			unbound = append(unbound, r)
		}
		if len(unbound) == 0 {
			continue
		}
		issues = append(issues, Issue{
			Severity: unboundSev, Category: "undefined_reference", File: p, Deterministic: unboundDet,
			// See the call-position check above: Signature keeps the two
			// undefined_reference findings for one file distinct.
			Signature: "unbound",
			Message:   renderUnboundRefs(p, unbound, jsBodies, exportIdx, declIdx, specIdx),
		})
	}

	// arity_mismatch: a same-module fixed-arity function called with more positional
	// arguments than it declares parameters. JS drops the extra - or shifts args into
	// the wrong slots (a value lands where a callback was expected, throwing `x is not
	// a function`) - so the bundle links clean and only an on-load probe path catches
	// it. The static catch for that class. One finding per file. A warning: far more
	// often the extra argument is simply ignored (a handler passed the event it does not
	// read), and a repair round spent "fixing" working calls is a round not spent on
	// what the owner can see.
	for _, p := range jsPaths {
		if jsBroken[p] {
			continue
		}
		mism := analysis.ArityOverApplications(p, jsBodies[p])
		if len(mism) == 0 {
			continue
		}
		issues = append(issues, Issue{
			Severity: store.SeverityWarning, Category: "arity_mismatch", File: p, Deterministic: true,
			Message: analysis.ExplainArityMismatches(p, mism),
		})
	}

	// module_specifier: a BARE import specifier resolving to neither a project module
	// nor a declared importmap library. esbuild treats every bare specifier as an
	// external "provided at runtime", so module_imports never flags it - yet the
	// browser throws at load, often swallowed into the app's own error screen so the
	// probe sees a "loaded" page. Skipped when the plan is unreadable (ok=false), so a
	// real library is never flagged "undeclared" for an unreadable plan.
	if libSpecs, ok := store.ModuleImportmapSpecifiers(pdb); ok {
		reserved := analysis.ReservedModulePaths()
		for _, u := range analysis.UnresolvedModuleSpecifiers(jsBodies, libSpecs) {
			if jsBroken[u.File] {
				continue
			}
			// A platform helper specifier ('auth', 'ofnet', 'e2ee') is never a library to
			// declare: when its module is missing, helper_import_missing says how to emit it.
			if _, isHelper := reserved[u.Spec]; isHelper {
				continue
			}
			// A COLLIDING specifier needs the opposite fix from an unknown one: two of
			// the project's own modules share this basename, so no bare name can ever
			// resolve it and "declare it as a library" / "import it by its bare logical
			// name" would both send the model in circles.
			msg := fmt.Sprintf("`%s` imports the bare module specifier `%s`, but nothing provides it - it is neither a project module nor a declared library, so the browser throws `Failed to resolve module specifier \"%s\"` at load (often swallowed into an app error screen, so a page probe misses it). Declare it in plan.external_libraries with `kind:\"module\"` and `module_name:\"%s\"` (manage_libraries add_library), or, if it is one of your own modules, import it by its bare logical name (the target file's basename without `js/` or the extension).", u.File, u.Spec, u.Spec, u.Spec)
			if u.Collides {
				msg = fmt.Sprintf("`%s` imports the bare module specifier `%s`, but TWO project modules share that basename, so it is ambiguous and left out of the importmap - the browser throws `Failed to resolve module specifier \"%s\"` at load. Do NOT declare it as a library. Rename one of the two files to a distinct basename (manage_files action=rename) and import that name, or import the one you mean by its relative path.", u.File, u.Spec, u.Spec)
			}
			issues = append(issues, Issue{
				Severity: store.SeverityError, Category: "module_specifier", File: u.File, Deterministic: true,
				Message: msg,
			})
		}
	}

	// js_binding: a binding-level error the bundler proves but a single-file syntax
	// parse cannot see - reassigning a `const`, assigning to an import, a duplicate
	// declaration. Mirrors the save-time static_error gate. Reuses the one bundle
	// above.
	for _, p := range jsPaths {
		if jsBroken[p] {
			continue
		}
		hard := facts.HardErrors(p)
		if len(hard) == 0 {
			continue
		}
		issues = append(issues, Issue{
			Severity: store.SeverityError, Category: "js_binding", File: p, Deterministic: true,
			Message: analysis.ExplainJSBindingError(p, hard),
		})
	}

	// dynamic_import: a name destructured from a literal dynamic import that the
	// target never exports - undefined at runtime. The static and missing-import
	// checks above only cover static `import {} from`, and esbuild's linker ignores
	// dynamic-import destructuring, so this is the static backstop for the probe.
	for _, p := range jsPaths {
		if jsBroken[p] {
			continue
		}
		var dynMiss []analysis.ImportIssue
		for _, is := range analysis.ResolveDynamicNamedImports(p, jsBodies, facts) {
			if is.Reason == "dynamic_name_missing" {
				dynMiss = append(dynMiss, is)
			}
		}
		if len(dynMiss) == 0 {
			continue
		}
		issues = append(issues, Issue{
			Severity: store.SeverityError, Category: "dynamic_import", File: p, Deterministic: true,
			Message: renderDynamicImportProblems(p, dynMiss, jsBodies),
		})
	}

	// module_cycle: a cyclic import graph. The save gate rejects the write that
	// closes a loop, but a cycle can span a sibling not saved yet at write time - this
	// is the whole-graph backstop. A cyclic page still boots, so the probe cannot see
	// it, yet eval order turns brittle and wiring can go silently dead. Deterministic
	// (FindCycle walks in sorted order). Reuses the one bundle above. A warning: the
	// page boots, and the explore probe reports any wiring the cycle actually kills.
	if cyc := facts.FindCycle(); len(cyc) > 0 {
		issues = append(issues, Issue{
			Severity: store.SeverityWarning, Category: "module_cycle", File: cyc[0], Deterministic: true,
			Message: renderModuleCycle(cyc),
		})
	}

	// js_syntax (inline <script>): a page/layout/component carries JS in inline
	// <script> blocks the path-keyed file checks above never see (a page's path is a
	// route, not a *.js file). Parse each block with the same esbuild gate. Mirrors
	// the save-time inlineScriptSyntax gate via analysis.InlineScripts. Deterministic
	// (documents and blocks walked in sorted/document order).
	if docs, derr := store.ArtifactList(pdb, store.ArtifactListOpts{Kinds: []string{"page", "layout", "component"}, IncludeBody: true}); derr == nil {
		sort.Slice(docs, func(i, j int) bool {
			if docs[i].Kind != docs[j].Kind {
				return docs[i].Kind < docs[j].Kind
			}
			return docs[i].Path < docs[j].Path
		})
		for _, a := range docs {
			if a == nil || a.Storage != store.StorageInline {
				continue
			}
			for _, s := range analysis.InlineScripts(a.Body) {
				if r := analysis.JSSyntax("inline.js", s.Code); r.Gated && !r.Clean {
					docLine := s.StartLine + r.FirstLine - 1
					issues = append(issues, Issue{
						Severity: store.SeverityError, Category: "js_syntax", Page: a.Path, Deterministic: true,
						Message: analysis.ExplainInlineScriptSyntaxError(a.Path, r.Detail, docLine),
					})
					break // one finding per document drives the fix
				}
			}
		}
	}
	return issues
}

// renderModuleCycle builds the actionable message for a cyclic import graph: the loop
// path plus the zero-import-leaf remedy. analysis.ExplainModuleCycle is the shared home,
// so this reads identically to the `check` action's finding.
func renderModuleCycle(cyc []string) string {
	return analysis.ExplainModuleCycle(cyc)
}

// renderDynamicImportProblems builds the actionable message for names a module
// destructures from a literal dynamic import that the target never exports, with a
// cross-module "did you mean" pointer when the name lives in another project module.
func renderDynamicImportProblems(file string, probs []analysis.ImportIssue, bodies map[string]string) string {
	index := analysis.ExportIndex(bodies)
	var b strings.Builder
	fmt.Fprintf(&b, "module `%s` destructures names from a dynamic import() that the target never exports - each is `undefined` at runtime and throws a `TypeError` when used:", file)
	for _, is := range probs {
		fmt.Fprintf(&b, "\n  - `import(%q)` destructures `%s`, but that module exports no such name", is.Spec, is.Imported)
		for _, src := range index[is.Imported] {
			if src == is.Target || src == file {
				continue
			}
			fmt.Fprintf(&b, " (it is exported by `%s`)", src)
			break
		}
	}
	b.WriteString("\nDestructure each name from the module that actually exports it, or add the export to the target.")
	return b.String()
}

// isJSModuleFile reports whether path is a JS module the import graph traverses.
// Mirrors the content write gate's isJSModulePath (.js / .mjs are the ESM family
// the renderer wires through the importmap).
func isJSModuleFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs":
		return true
	}
	return false
}

// renderUndefinedRefs builds the actionable message for names a module uses in
// call position but never declares, imports, or shares with the globals allowlist.
// The per-name steer is analysis.ExplainUndefinedRef, shared with the save gate; this
// supplies only the framing this surface needs.
func renderUndefinedRefs(file string, refs []analysis.UndefinedRef, exportIdx, declIdx map[string][]string, specIdx map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module `%s` uses name(s) that are neither declared in it, imported, nor a known browser/JS global - the browser throws `<name> is not defined` when that code path runs (a page-load probe misses it when the call sits in an event handler):", file)
	for _, r := range refs {
		fmt.Fprintf(&b, "\n  - `%s` (line %d): %s", r.Name, r.Line, analysis.ExplainUndefinedRef(file, r, exportIdx, declIdx, specIdx))
	}
	b.WriteString("\nFix each name, then rebuild.")
	return b.String()
}

// renderUnboundRefs describes names no enclosing scope binds, reached through a
// member or an assignment - the two shapes a call-position scan cannot see. It
// prefers the library steer over the project-export one, exactly as the save
// gate does: the export lookup cannot resolve a library binding and would
// otherwise say only "define it in this module".
func renderUnboundRefs(file string, refs []analysis.UndefinedRef, bodies map[string]string, exportIdx, declIdx map[string][]string, specIdx map[string]string) string {
	externals := analysis.ExternalBindingsElsewhere(bodies, file)
	var b strings.Builder
	fmt.Fprintf(&b, "module `%s` uses name(s) no enclosing scope binds - neither declared in it, imported, nor a known browser/JS global. The browser throws `<name> is not defined` when that code path runs, and an interaction-only path (a resize handler, a tick callback) never runs under the page-load probe:", file)
	for _, r := range refs {
		steer := analysis.ExplainUnboundExternalRef(r, externals)
		if steer == "" {
			steer = analysis.ExplainUndefinedRef(file, r, exportIdx, declIdx, specIdx)
		}
		fmt.Fprintf(&b, "\n  - `%s` (line %d): %s", r.Name, r.Line, steer)
	}
	return b.String()
}

func init() {
	RegisterValidator(staticCodeValidator{})
}
