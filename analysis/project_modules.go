package analysis

import (
	"path"
	"sort"
	"strings"
)

// Project-module wiring: the PLATFORM, not the model, owns the import graph for
// a project's own JS modules. Each internal module is exposed under a bare
// logical specifier (basename minus extension) that the renderer maps to the
// real /files path via the page importmap, so the model never hand-authors a
// relative path. ResolveImport still handles relative-path projects, so this is
// purely additive. See server/public/render.go (loadImportMap, importedModulePaths).

// reservedModulePaths maps each platform-reserved bare specifier to the fixed
// project path the platform emits its helper at. The names are the platform's
// to keep: a user module must not claim one. The save gate rejects such a save
// (tools/content/manage_files_write.go), and ProjectModuleMap resolves the
// reserved name to the platform path even on a stray basename collision so the
// helper import never silently breaks.
var reservedModulePaths = map[string]string{
	"ofnet": "js/ofnet.js",
	"e2ee":  "js/e2ee.js",
	"auth":  "js/auth.js",
}

// IsReservedModuleSpecifier reports whether name is a platform-reserved bare
// module specifier that a user module may not define.
func IsReservedModuleSpecifier(name string) bool {
	_, ok := reservedModulePaths[name]
	return ok
}

// ReservedModulePaths returns a copy of the specifier -> platform file map, so a
// caller that must reason about the helpers (which one backs which import, is it
// emitted yet) reads the one definition rather than restating it.
func ReservedModulePaths() map[string]string {
	out := make(map[string]string, len(reservedModulePaths))
	for k, v := range reservedModulePaths {
		out[k] = v
	}
	return out
}

// ProjectModuleName derives the bare logical specifier for an internal JS module
// from its project path: the basename with its extension removed.
//
//	"js/world.js"      -> "world"
//	"js/world-gen.js"  -> "world-gen"
//	"main.mjs"         -> "main"
//
// A path with no usable basename (empty, "/", ".") yields "".
func ProjectModuleName(p string) string {
	base := path.Base(p)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	if ext := path.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// projectModuleSpecifierForms returns every bare specifier a module path answers
// to: its logical name (basename minus extension) and its full basename.
//
//	"js/world.js"      -> ["world", "world.js"]
//	"club.state.js"    -> ["club.state", "club.state.js"]
//
// The basename form exists because the logical name strips only the LAST
// extension: on a dotted filename a model that imports the filename it just
// wrote would otherwise resolve nowhere. The two form-spaces cannot overlap (a
// logical name never keeps an extension, a basename always has one), and both
// forms go through ProjectModuleMap's collision counting, so a genuinely
// ambiguous name is dropped in either form.
func projectModuleSpecifierForms(p string) []string {
	name := ProjectModuleName(p)
	if name == "" {
		return nil
	}
	if base := path.Base(p); base != name {
		return []string{name, base}
	}
	return []string{name}
}

// reservedPathForForm returns the platform path a reserved specifier names, in
// either form ("ofnet" or "ofnet.js").
func reservedPathForForm(name string) (string, bool) {
	for rn, rp := range reservedModulePaths {
		if rn == name || path.Base(rp) == name {
			return rp, true
		}
	}
	return "", false
}

// ProjectModuleMap builds the specifier -> project-path index over a set of JS
// module paths, indexing each module under both forms
// (projectModuleSpecifierForms). An ambiguous specifier (two modules sharing a
// basename) cannot be exposed as a single bare name, so it is LEFT OUT of
// byName and reported in collisions; those modules remain reachable by relative
// path. This is the single index the save gate, the post-build validator, and
// the renderer's importmap all resolve through - anything that narrows or
// widens what resolves belongs here, in one place, never in one consumer.
func ProjectModuleMap(jsPaths []string) (byName map[string]string, collisions []string) {
	counts := make(map[string]int)
	first := make(map[string]string)
	for _, p := range jsPaths {
		for _, name := range projectModuleSpecifierForms(p) {
			if counts[name] == 0 {
				first[name] = p
			}
			counts[name]++
		}
	}
	byName = make(map[string]string, len(counts))
	for name, n := range counts {
		if n == 1 {
			byName[name] = first[name]
			continue
		}
		// A platform-reserved name resolves to the platform path even on a
		// collision, so a stray user module sharing the basename cannot knock the
		// helper out of the importmap. (The save gate normally rejects such a user
		// save before it lands; this is defense in depth.)
		if rp, ok := reservedPathForForm(name); ok {
			hasPlatformPath := false
			for _, p := range jsPaths {
				if p == rp {
					hasPlatformPath = true
					break
				}
			}
			if hasPlatformPath {
				byName[name] = rp
				continue
			}
		}
		collisions = append(collisions, name)
	}
	sort.Strings(collisions)
	return byName, collisions
}

// ResolveProjectModuleSpecifier returns the project path a bare import
// specifier refers to, if it names an internal module present in byName.
// Relative and web-absolute specifiers are ResolveImport's job and return
// ok=false here, as does a bare specifier naming no internal module (e.g. an
// external library), so an external lib is never mistaken for an internal one.
func ResolveProjectModuleSpecifier(spec string, byName map[string]string) (string, bool) {
	if IsRelativeSpecifier(spec) || strings.HasPrefix(spec, "/") {
		return "", false
	}
	p, ok := byName[spec]
	return p, ok
}

// CollidingModuleName returns the bare specifier that saving newPath would make
// ambiguous, or "" when the name stays resolvable. existingPaths is the project's
// other live JS module paths; newPath itself may appear in it (a re-save of the same
// file is never a collision with itself).
//
// It answers the save-time question with the SAME index the importmap and the
// post-build validator resolve through, so a gate that refuses a name and a runtime
// that drops it can never disagree about which names are ambiguous.
func CollidingModuleName(existingPaths []string, newPath string) string {
	if ProjectModuleName(newPath) == "" {
		return ""
	}
	paths := make([]string, 0, len(existingPaths)+1)
	seen := map[string]bool{}
	for _, p := range append(append([]string{}, existingPaths...), newPath) {
		if seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	_, collisions := ProjectModuleMap(paths)
	mine := map[string]bool{}
	for _, form := range projectModuleSpecifierForms(newPath) {
		mine[form] = true
	}
	for _, c := range collisions {
		if mine[c] {
			return c
		}
	}
	return ""
}

// CollidingModulePaths returns every path in paths that answers to the bare specifier
// name, so a rejection can NAME the file the caller has to rename.
func CollidingModulePaths(paths []string, name string) []string {
	var out []string
	for _, p := range paths {
		for _, form := range projectModuleSpecifierForms(p) {
			if form == name {
				out = append(out, p)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
