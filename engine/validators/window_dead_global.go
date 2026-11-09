package validators

// The dead-window-global validator: catches a `window.<name>` some project file
// READS but NO project file ever ASSIGNS - a silently dead cross-module bridge.
// Fills the blind spot between undefined_reference (a member read on the real
// `window` global throws nothing) and the save-time window-bridge gate (a read
// written nowhere has no second side); the read just evaluates to `undefined`.
// A `__`-prefixed dead read is the platform/project bridge convention, so it is
// error+deterministic; a BARE read might be a UMD library global, so it follows
// the library-aware unboundRefSeverity. Not blocking: a dead read still ships a
// usable app, so it drives a fix-pass. Dot-form only, the
// miss-over-false-positive bias every window check holds.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"openfabrica/store"
)

type windowDeadGlobalValidator struct{}

func (windowDeadGlobalValidator) Name() string { return "window_dead_global" }

func (windowDeadGlobalValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	files := jsAndPageFiles(pdb)
	if len(files) == 0 {
		return nil
	}

	// One pass over every project JS/page body: union the producers (LHS of
	// `window.X =`) and, per file, the non-allowlisted consumers. A name assigned in
	// ANY file is wired (maybe a smell the bridge gate owns, but not dead), so only
	// names with zero producers anywhere are candidates here.
	producers := map[string]bool{}
	readsByFile := map[string][]string{} // file -> names it reads (non-allowlisted)
	for _, f := range files {
		all, prod := store.WindowGlobalRefs(f.body)
		for name := range prod {
			producers[name] = true
		}
		for name := range all {
			if store.IsAllowedWindowGlobal(name) {
				continue // platform / browser / language global, legitimately read
			}
			readsByFile[f.path] = append(readsByFile[f.path], name)
		}
	}

	// Library-aware severity for BARE names (a `window.Foo` might be an unnamed UMD
	// global); `__`-prefixed names are never that and stay error+deterministic.
	libSev, libDet, libGlobals := unboundRefSeverity(pdb)
	// A global a declared classic <script src> defines IS assigned at runtime - by the
	// library, in a file the project does not own - so it is a producer, not a dead
	// read. Naming it in the plan is what turns "might be a UMD global" into a fact,
	// and turns every OTHER unproduced bare name into a blocking error below.
	for name := range libGlobals {
		producers[name] = true
	}

	var issues []Issue
	paths := make([]string, 0, len(readsByFile))
	for p := range readsByFile {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		var dead []string
		hasUnderscore := false
		seen := map[string]bool{}
		for _, name := range readsByFile[p] {
			if producers[name] || seen[name] {
				continue
			}
			seen[name] = true
			dead = append(dead, name)
			if strings.HasPrefix(name, "__") {
				hasUnderscore = true
			}
		}
		if len(dead) == 0 {
			continue
		}
		sort.Strings(dead)
		sev, det := libSev, libDet
		if hasUnderscore {
			sev, det = store.SeverityError, true
		}
		is := Issue{
			Severity: sev, Category: "window_dead_global", Deterministic: det,
			Message: renderDeadWindowGlobals(p, dead, !hasUnderscore),
		}
		// A page's path is a route, not a *.js file; keep the Issue's location field
		// consistent with the other validators (File for modules, Page for documents).
		if isJSModuleFile(p) {
			is.File = p
		} else {
			is.Page = p
		}
		issues = append(issues, is)
	}
	return issues
}

// renderDeadWindowGlobals builds the actionable message: name the dead reads and
// steer to the same zero-import-leaf export/import fix the bridge gate uses, since
// the remedy is identical - the symbol belongs in a module every reader imports, not
// on `window`. When every name is bare it may instead be the global of a declared
// classic library whose global_name is missing or wrong; that case is named first,
// because its fix is a declaration change, and rewriting working code to satisfy
// the check is what breaks the feature.
func renderDeadWindowGlobals(file string, names []string, maybeLibrary bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module `%s` reads `window.<name>` symbol(s) that NO project file ever assigns - a dead cross-module bridge. `window` is a real global, so the read throws nothing; it silently evaluates to `undefined`, so the feature it drives never works, and no import or undefined-reference gate can see it (there is no import edge, and the base `window` is defined):", file)
	for _, n := range names {
		fmt.Fprintf(&b, "\n  - `window.%s` is read here but assigned in no project file", n)
	}
	if maybeLibrary {
		b.WriteString("\nFIRST rule out a library global: if a declared classic <script> library defines this name (check its docs), the code is correct and must NOT be changed. " +
			"Fix the declaration instead: `manage_libraries action=add_library` with that library's name and `global_name` set to the exact global it defines. " +
			"Only when no library defines it:")
	}
	b.WriteString("\nMove each symbol into the module that owns it: `export` it from a zero-import store module and `import` it here - never bridge it on `window`. When the value is one an already-built sibling leaf computes, add the `export` to that leaf (a small patch) and import it. Only `window.__routeParams` / `window.__ofProbeViolations` (platform-injected) are exempt window reads.")
	return b.String()
}

func init() {
	RegisterValidator(windowDeadGlobalValidator{})
}
