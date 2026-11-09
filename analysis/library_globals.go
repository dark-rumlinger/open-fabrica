package analysis

// Which ambient globals a project's declared CDN libraries supply - the one
// decision that tells every unbound-reference gate whether an unresolved
// `name.prop` / `name = ...` is a REAL defect or a library global the platform
// simply cannot see.
//
// Only a classic UMD/IIFE `<script src>` defines an ambient global. An importmap
// ESM library is reachable only by `import`, a stylesheet/font defines no JS
// binding at all, so neither can supply one. When every classic library names the
// global it defines, the answer is EXACT: any other unbound name is genuinely
// undefined and the gates may block on it. A classic library that names nothing
// leaves one hole, and the whole project falls back to non-blocking.
//
// Shared by the save-time write gate (tools/content) and the round-level static
// validator (engine/validators) so the two cannot drift apart, the same way
// IsJSGlobal single-sources the browser/JS allowlist.

import "strings"

// ExternalLib is the subset of a plan's `external_libraries` entry this decision
// needs. Declared here rather than taken from the engine so analysis stays free of
// internal dependencies; callers convert their own spec type into it.
type ExternalLib struct {
	Name       string // display name, for messages only
	URL        string // CDN URL; classification falls back to Kind when empty
	Kind       string // "classic" (UMD/IIFE <script src>) or "module" (importmap ESM)
	ModuleName string // bare import specifier, module libraries only
	// GlobalName is the `window.<name>` a classic script defines (`marked`,
	// `Chart`, `qrcode`). Empty on a classic library is the one case that makes
	// the whole answer inexact.
	GlobalName string
}

// LibraryDefinesGlobal reports whether this library can define an ambient global,
// mirroring the renderer's injection decision: only a library that lands in its
// classic `<script src>` branch can. Conservative - an unknown shape assumes it can.
func LibraryDefinesGlobal(l ExternalLib) bool {
	if strings.EqualFold(strings.TrimSpace(l.Kind), "module") {
		return false // importmap ESM: reachable only by `import`, defines no global
	}
	u := strings.TrimSpace(l.URL)
	if u == "" {
		return true // unknown shape: assume it could
	}
	switch {
	case ExternalURLIsCSS(u), ExternalURLIsFontFile(u):
		return false // <link>: defines no JS binding at all
	case ExternalURLIsESM(u):
		return false // the renderer emits type="module" for these, global-free
	}
	return true // classic <script src>: defines a global
}

// LibraryGlobals returns the ambient globals the declared libraries supply, and
// whether that set is EXACT - i.e. whether every library that could define a
// global actually named it.
//
// exact=true licenses a gate to BLOCK on an unbound name: nothing undeclared can
// be supplying it. exact=false means at least one classic library is an unknown
// quantity, so an unbound name might be its global and a gate must not block.
// A project with no globals-capable library is trivially exact (empty set).
//
// Names are returned bare (`qrcode`), matching how a module references them both
// as `window.qrcode` and as a free identifier. Pure and deterministic.
func LibraryGlobals(libs []ExternalLib) (globals map[string]bool, exact bool) {
	globals = map[string]bool{}
	exact = true
	for _, l := range libs {
		if !LibraryDefinesGlobal(l) {
			continue
		}
		name := strings.TrimSpace(l.GlobalName)
		// A `window.`-qualified or dotted name is tolerated: the leading segment is
		// the binding the script actually creates, which is what a gate compares to.
		name = strings.TrimPrefix(name, "window.")
		if i := strings.Index(name, "."); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			exact = false // a classic script whose global the platform cannot name
			continue
		}
		globals[name] = true
	}
	return globals, exact
}
