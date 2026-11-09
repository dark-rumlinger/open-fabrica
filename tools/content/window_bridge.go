package content

// Cross-module window-global bridge write gate for manage_files. The cyclic and
// unresolved import gates (unresolved_imports.go) see only import EDGES; a leaf
// that reaches a sibling's symbol through `window.__gameKeys` instead of an
// `import` carries no edge, so it sails past them and ships wiring that resolves
// nowhere statically: the global is read before (or without) ever being written,
// the feature silently dead while every gate and probe stays green. Like the
// cyclic gate, it fires on the edit that CLOSES the bridge, from either direction,
// and subtracts any bridge already present so an in-progress repair is never
// trapped. (Enforces the client_js_split guide's "Never `window.__` globals".)
//
// Scope is ANY project-authored `window.<name>` property, not just the `__`
// convention: a bare `window.WA` namespace shared across leaves (a docking
// manager, a game bus) dodges the import graph the same way, so
// `window.WA.dockTogether()` written before that method exists must fail at write,
// not at the runtime probe. The catch is the name after `window.`, with a
// false-positive-safe allowlist (isAllowedWindowGlobal) for the platform's reserved
// runtime injections (`window.__routeParams`, `window.__ofProbeViolations`) and
// browser/BOM built-ins (`window.location`, `window.addEventListener`) that pages
// are MEANT to share. (Auth/realtime/crypto helpers are ES modules imported by
// specifier now, not window globals.) Only a project's OWN `window.<name>` shared
// across TWO OR MORE JS files fires; one confined to a single file never does.

import (
	"fmt"
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/store"
	"openfabrica/tools"
)

// The `window.<name>` scan itself is shared: store.WindowGlobalRefs is the single
// home for the pattern (dot form only; `window.WA.dockTogether` yields `WA`, the
// shared namespace, not the method). The allowlist below, not the scan, distinguishes
// a project-authored bridge from a platform/browser global.

// The window-global allowlist (reserved platform names, browser/BOM built-ins,
// language globals) now lives in ONE home next to the scan it filters:
// store.IsAllowedWindowGlobal. Both this save-time bridge gate and the post-build
// dead-global validator classify a `window.<name>` the same way through it, so
// "a project-authored bridge" cannot mean different things on the write and build
// paths. The reserved-name drift test (TestReservedWindowGlobals_StillInjected)
// reads that home via store.ReservedWindowGlobalNames.

// bridgedWindowGlobals returns each project-authored `window.<name>` referenced in
// TWO OR MORE distinct project JS files, mapped to the sorted set of files
// referencing it. Two files sharing a `window.<name>` global communicate through the
// global object instead of the module import graph - the pattern that slips past the
// import gates. Platform-injected and browser/BOM globals (isAllowedWindowGlobal) are
// reads pages legitimately share and are omitted. A NAME confined to a single file is
// not a cross-module bridge and is omitted. Direction (write vs read) is intentionally
// not distinguished: any two files touching the same symbol are coupled through it
// regardless of which assigns and which reads.
func bridgedWindowGlobals(bodies map[string]string) map[string][]string {
	files := map[string]map[string]bool{} // name -> set of files referencing it
	for path, body := range bodies {
		if !isJSModulePath(path) {
			continue
		}
		all, _ := store.WindowGlobalRefs(body)
		for name := range all {
			if store.IsAllowedWindowGlobal(name) {
				continue // platform / browser global, read legitimately across files
			}
			if files[name] == nil {
				files[name] = map[string]bool{}
			}
			files[name][path] = true
		}
	}
	out := map[string][]string{}
	for name, set := range files {
		if len(set) < 2 {
			continue
		}
		fs := make([]string, 0, len(set))
		for f := range set {
			fs = append(fs, f)
		}
		sort.Strings(fs)
		out[name] = fs
	}
	return out
}

// windowBridgeRejection refuses the write that CLOSES a cross-module `window.__`
// bridge through path. Mirrors cycleRejection's false-positive-safe asymmetry: it
// fires only on the edit that closes the bridge - a bridge already present against
// oldBody is an in-progress state this edit did not create, left to proceed - and
// only when path is itself one side of the bridge, so an unrelated save is never
// blamed for two other files' shared global. Returns nil when there is nothing
// fresh to flag.
func windowBridgeRejection(path, oldBody string, bodies map[string]string) *tools.Result {
	now := bridgedWindowGlobals(bodies)
	if len(now) == 0 {
		return nil
	}
	var prior map[string][]string
	if oldBody != "" {
		prior = bridgedWindowGlobals(withOverlay(bodies, path, oldBody))
	}
	// Collect EVERY fresh bridge this write closes where `path` is one side, not just
	// the first. A save that reaches for several `window.<name>` symbols (or one shared
	// with more than one sibling) is told about all of them, and all their readers, at
	// once - so the model relocates the whole shared surface in ONE turn instead of
	// tripping this gate once per symbol (a relocate-one-at-a-time loop).
	// Deterministic: smallest name first.
	names := make([]string, 0, len(now))
	for name := range now {
		names = append(names, name)
	}
	sort.Strings(names)
	fresh := map[string][]string{}
	order := make([]string, 0, len(names))
	for _, name := range names {
		files := now[name]
		if !listed(files, path) {
			continue // a bridge between OTHER files; not this write's doing
		}
		if len(prior[name]) > 0 {
			continue // pre-existing bridge, not introduced by this write
		}
		fresh[name] = files
		order = append(order, name)
	}
	if len(order) == 0 {
		return nil
	}
	return windowBridgeResult(path, order, fresh)
}

// windowBridgeResult builds the reject. It lists EVERY file that currently reads each
// bridged symbol and states the one invariant models most often miss on the fix:
// exactly ONE zero-import store module may read `window.<name>`, and every other reader
// must be converted to an `import` IN THE SAME turn. A "wrapper" file that still leaves
// the original readers touching `window.<name>` just re-trips the gate - a multi-reject
// loop. Steers to the same store-module fix as the cyclic gate (the
// symbol belongs in a shared store module every file imports, not on `window`).
func windowBridgeResult(path string, names []string, fresh map[string][]string) *tools.Result {
	var list strings.Builder
	for _, name := range names {
		readers := make([]string, 0, len(fresh[name]))
		for _, f := range fresh[name] {
			readers = append(readers, baseName(f))
		}
		fmt.Fprintf(&list, "\n  - `window.%s` is read across `%s`", name, strings.Join(readers, "`, `"))
	}
	return tools.FailCode("window_global_bridge",
		"write rejected: `%s` passes %s between modules on the global object instead of through the import graph. The artifact was not modified. The cyclic_import and unresolved_import gates cannot see a `window.*` read (there is no import edge), so it ships wiring that resolves nowhere statically and can boot silently dead - the global is read (a method on it called) before, or without, ever being assigned.%s\nFix in ONE turn: put each symbol - or, for a CDN library global like `window.marked`, a single re-export of it - in a zero-import store module (a file that imports no other project module), `export` it there, and change EVERY file listed above to `import` it from that module. Exactly ONE file may still read `window.<name>`: that store module. Adding another reader - a second wrapper that itself reads `window.<name>` while the originals still do - re-trips this gate. The platform HELPERS are ES modules, not window globals - import them (`import { apiFetch } from 'auth'`, `import { connect } from 'ofnet'`, `import { session } from 'e2ee'`); only the router/probe injections (`window.__routeParams`, `window.__ofProbeViolations`) are exempt window reads. Then retry.",
		baseName(path), bridgePhrase(names), list.String())
}

// The dead-window-global side of the same family: a `window.__x` READ that nothing
// assigns. The bridge gate above needs the symbol on two sides, so a read written
// nowhere slips past it. Save-time twin of the window_dead_global validator, sharing
// its code, scan and allowlist.

// deadWindowCandidates returns the `__`-prefixed, non-allowlisted `window.<name>`
// symbols body reads. `__` only, matching the validator's severity split: a bare
// `window.Foo` may come from a classic script tag the static scan cannot see.
func deadWindowCandidates(body string) []string {
	all, _ := store.WindowGlobalRefs(body)
	var out []string
	for name := range all {
		if !strings.HasPrefix(name, "__") || store.IsAllowedWindowGlobal(name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// storedWindowProducers unions `window.<name> =` across every stored inline artifact
// outside bodies: pages, layouts and components too, not just JS modules. Scanned as
// wide as possible because a missed producer reads as "assigned nowhere".
func storedWindowProducers(ctx *tools.ToolContext, bodies map[string]string) map[string]bool {
	out := map[string]bool{}
	if ctx == nil || ctx.DB == nil {
		return out
	}
	arts, _ := store.ArtifactList(ctx.DB, store.ArtifactListOpts{IncludeBody: true})
	for _, a := range arts {
		if a.Storage != store.StorageInline {
			continue
		}
		// bodies holds file-kind JS only, so the overlay owns just those rows.
		if _, ok := bodies[a.Path]; ok && a.Kind == "file" {
			continue
		}
		_, prod := store.WindowGlobalRefs(a.Body)
		for name := range prod {
			out[name] = true
		}
	}
	return out
}

// windowProducers adds every in-flight body's own assignments to the stored set.
func windowProducers(bodies map[string]string, stored map[string]bool) map[string]bool {
	out := make(map[string]bool, len(stored))
	for name := range stored {
		out[name] = true
	}
	for _, body := range bodies {
		_, prod := store.WindowGlobalRefs(body)
		for name := range prod {
			out[name] = true
		}
	}
	return out
}

// windowDeadGlobalRejection refuses the write that INTRODUCES a `window.__<name>` read
// no project artifact assigns. Same false-positive-safe asymmetry as
// windowBridgeRejection: only names `path` itself reads count, and a name already dead
// against oldBody is a pre-existing state this edit did not create. Because it fires
// only on names the written file READS, dropping an assignment from a producer file
// while a sibling still reads it is never trapped here.
func windowDeadGlobalRejection(ctx *tools.ToolContext, path, oldBody string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	// Cheap pre-check first: no `__` read in the written body means no project scan.
	cands := deadWindowCandidates(bodies[path])
	if len(cands) == 0 {
		return nil
	}
	stored := storedWindowProducers(ctx, bodies)
	produced := windowProducers(bodies, stored)

	var dead []string
	for _, name := range cands {
		if !produced[name] {
			dead = append(dead, name)
		}
	}
	if len(dead) == 0 {
		return nil
	}
	if oldBody != "" {
		priorProduced := windowProducers(withOverlay(bodies, path, oldBody), stored)
		prior := map[string]bool{}
		for _, name := range deadWindowCandidates(oldBody) {
			if !priorProduced[name] {
				prior[name] = true
			}
		}
		var fresh []string
		for _, name := range dead {
			if !prior[name] {
				fresh = append(fresh, name)
			}
		}
		dead = fresh
	}
	if len(dead) == 0 {
		return nil
	}
	return windowDeadGlobalResult(path, dead, bodies, facts)
}

// windowDeadGlobalResult builds the reject. It names every dead symbol at once, blocks
// the obvious wrong fix (adding the missing assignment, which just closes a bridge the
// next gate rejects), and carries the setter idiom for the case that produces this most
// often: a value only the page entry can compute at boot.
func windowDeadGlobalResult(path string, names []string, bodies map[string]string, facts *analysis.ModuleFacts) *tools.Result {
	var list strings.Builder
	for _, name := range names {
		fmt.Fprintf(&list, "\n  - `window.%s` is read here and assigned in no project file, page, layout or component", name)
	}
	// The example symbol for the setter idiom. A name of exactly `__` strips to empty,
	// so fall back rather than index it.
	sym := strings.TrimPrefix(names[0], "__")
	if sym == "" {
		sym = "value"
	}

	msg := fmt.Sprintf(
		"write rejected: `%s` reads %s that NOTHING in the project ever assigns - a dead bridge, not a wire. The artifact was not modified. `window` is a real global, so the read throws nothing: it evaluates to `undefined`, the feature it drives silently does nothing, and the import gates, the module graph and the page-load probe all stay green. A `window.__*` name is never a CDN library global, so nothing else can be supplying it.%s",
		baseName(path), bridgePhrase(names), list.String())

	msg += "\nFix in ONE turn, and do NOT fix it by adding the missing `window." + names[0] + " = ...` assignment somewhere - that only turns a dead read into a cross-module window bridge, which is rejected too. "

	if dest := cycleDest(path, path, bodies, facts); dest != "" {
		msg += fmt.Sprintf(
			"Put each symbol in the zero-import store module `%s` (a file that imports no other project module), `export` it there, and `import` it here from '%s'.",
			baseName(dest), analysis.SuggestImportSpecifier(path, dest, analysis.ModuleSpecifierIndex(bodies)))
	} else {
		msg += "Put each symbol in a zero-import store module (a file that imports no other project module), `export` it there, and `import` it here."
	}

	msg += fmt.Sprintf(
		" When the value is one only the page ENTRY can compute at boot (a camera, a renderer, a projector, a socket), the store module declares a live binding plus a one-line setter - `export let %s = null; export function set%s(v){ %s = v; }` - the entry calls the setter once at boot, and this file reads the imported name directly (a live binding is current after boot). Guarding the read (`if (window.%s)`) does not make it wired, it makes the dead path permanent. Then retry.",
		sym, strings.ToUpper(sym[:1])+sym[1:], sym, names[0])

	return tools.FailCode("window_dead_global", "%s", msg)
}

// bridgePhrase renders the bridged-symbol summary for the reject's opening line: a
// single `window.name`, or a comma-joined list when one save closes several bridges.
func bridgePhrase(names []string) string {
	qs := make([]string, len(names))
	for i, n := range names {
		qs[i] = "`window." + n + "`"
	}
	return strings.Join(qs, ", ")
}

// listed reports whether s is in xs (a small local helper; baseName in this file
// takes the same minimal-dependency approach).
func listed(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
