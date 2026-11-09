package analysis

// Cross-module import/export resolution. The single-file JS gate (JSSyntax) only
// proves a module PARSES; it cannot see that `import { state } from './x.js'` names
// an export './x.js' never declares, since that is a property of the whole module
// graph, not one file - the structural blind spot the runtime probe otherwise
// catches stages later. These helpers answer the graph question statically over
// the same path space the renderer resolves, so the write gate can refuse the
// break at save time.
//
// Pure data in, pure data out: callers pass `bodies` (project path -> JS source,
// INCLUDING the in-flight write overlaid onto the focus path) so the analysis
// reflects what the write is about to store. All bodies are comment-stripped
// (string literals preserved, via StripJSComments) before scanning, so a
// commented-out import or export never registers - mandatory for a hard gate -
// while the specifiers themselves survive intact.

import (
	"regexp"
	"sort"
)

// exportDefaultRe matches a default export in any form (`export default x`,
// `export default function`, `export default class`). Run on comment-stripped
// code so a `// export default` comment never matches.
var exportDefaultRe = regexp.MustCompile(`(?m)\bexport\s+default\b`)

// StripJSComments removes line (`//`) and block (`/* */`) comments while copying
// string and template literals verbatim. This is the right sanitizer for
// import/export scanning - maskJS blanks string INTERIORS too, which would erase
// the very specifier (`'./x.js'`) the resolver needs. A `//` inside a string is
// preserved (it is not a comment); the rare `${...}` template expression is left
// as-is, harmless for the top-of-file import region this scans. Regex literals are
// recognized and copied verbatim (they are code), so a regex whose body holds a
// quote or `//` - e.g. `.replace(/'/g, ”)` or `/[//]/` - is not mistaken for a
// string or a comment, which would desync parsing for the rest of the file.
func StripJSComments(src string) string {
	out := make([]byte, 0, len(src))
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			} else {
				i = n
			}
		case c == '/' && regexFollows(out):
			if rl := scanRegexLiteral(src, i); rl > 0 {
				out = append(out, src[i:i+rl]...) // copy the regex literal verbatim
				i += rl
			} else {
				out = append(out, c) // division: an ordinary operator
				i++
			}
		case c == '\'' || c == '"' || c == '`':
			out = append(out, c)
			i++
			for i < n {
				d := src[i]
				out = append(out, d)
				i++
				if d == '\\' && i < n {
					out = append(out, src[i])
					i++
					continue
				}
				if d == c {
					break
				}
			}
		default:
			out = append(out, c)
			i++
		}
	}
	return string(out)
}

// ImportIssue describes one named import in the focus module the project's export
// surface does not satisfy. Reason is "name_missing" when the target module
// EXISTS but exports no such name (a real contract break), or "target_absent"
// when no project module resolves the specifier yet (the normal mid-split case,
// which the caller treats as advisory, never a reject). Exports/HasDefault are
// populated for name_missing only, so the caller can render an actionable
// "did you mean" steer.
type ImportIssue struct {
	Spec       string
	Imported   string
	Local      string
	Target     string
	Reason     string
	Exports    []string
	HasDefault bool
}

// ImporterBreak names a saved module that imports a specific name from a module
// whose pending write removes that export - the producer-side mirror of a
// dangling import.
type ImporterBreak struct {
	Importer string
	Imported string
	Spec     string
}

// resolveImportTarget classifies a specifier from importer against the project
// module set. internal is true for a project-internal reference (relative,
// web-absolute, or a bare specifier that names an internal module); present is
// true when that reference resolves to a member of bodies. An external library
// specifier (bare, not an internal module) returns internal=false so the caller
// skips it - the importmap, not the project graph, owns those.
func resolveImportTarget(importer, spec string, bodies map[string]string, byName map[string]string) (target string, present, internal bool) {
	if IsRelativeSpecifier(spec) || (len(spec) > 0 && spec[0] == '/') {
		base, ok := ResolveImport(importer, spec)
		if !ok {
			return "", false, false
		}
		if r := resolveInSet(base, bodies); r != "" {
			return r, true, true
		}
		return base, false, true
	}
	if internalPath, ok := ResolveProjectModuleSpecifier(spec, byName); ok {
		return internalPath, true, true
	}
	return "", false, false
}

// ResolveNamedImportsWith is ResolveNamedImports against a precomputed
// ModuleFacts for the same bodies, so a caller checking many focus modules over
// one body set (the post-build validator) bundles once instead of per file.
func ResolveNamedImportsWith(focus string, bodies map[string]string, facts *ModuleFacts) []ImportIssue {
	body, ok := bodies[focus]
	if !ok {
		return nil
	}
	imports := ExtractNamedImports(StripJSComments(body))
	if len(imports) == 0 {
		return nil
	}
	paths := make([]string, 0, len(bodies))
	for p := range bodies {
		paths = append(paths, p)
	}
	byName, _ := ProjectModuleMap(paths)

	// Per-target export cache: a focus that pulls several names from one sibling
	// parses that sibling once.
	// Per-target surface cache: a focus pulling several names from one sibling
	// parses that sibling once. Names/hasDef feed the actionable message only; the
	// satisfied-or-not decision is the linker's (ModuleFacts.Missing), gated by
	// whether the target's export surface is statically knowable (ExportsComplete).
	type exp struct {
		names  []string
		hasDef bool
	}
	cache := map[string]exp{}
	exportsOf := func(target string) exp {
		if e, ok := cache[target]; ok {
			return e
		}
		var e exp
		if tb, present := bodies[target]; present {
			code := StripJSComments(tb)
			e.names = ExtractNamedExports(code)
			e.hasDef = exportDefaultRe.MatchString(code)
		}
		cache[target] = e
		return e
	}

	var out []ImportIssue
	seen := map[string]bool{}
	for _, ni := range imports {
		key := ni.Spec + "\x00" + ni.Imported
		if seen[key] {
			continue
		}
		seen[key] = true

		target, present, internal := resolveImportTarget(focus, ni.Spec, bodies, byName)
		if !internal {
			continue // external library: importmap's concern, not the graph's
		}
		if !present {
			out = append(out, ImportIssue{Spec: ni.Spec, Imported: ni.Imported, Local: ni.Local, Target: target, Reason: "target_absent"})
			continue
		}
		if !facts.ExportsComplete(target) {
			continue // target (transitively) re-exports * from an opaque source: stay silent
		}
		if !facts.Missing(focus, target, ni.Imported) {
			continue // the linker resolved it (a direct export, alias, or re-export chain)
		}
		e := exportsOf(target)
		out = append(out, ImportIssue{
			Spec: ni.Spec, Imported: ni.Imported, Local: ni.Local, Target: target,
			Reason: "name_missing", Exports: e.names, HasDefault: e.hasDef,
		})
	}
	return out
}

// ResolveDynamicNamedImports checks names a focus module destructures from a
// LITERAL dynamic import (`import('./x.js').then(({a})=>...)` or
// `const {a} = await import('./x.js')`) against that target's export surface.
// esbuild's linker treats a dynamic import as a runtime value and never checks its
// destructuring, and the static gate (ResolveNamedImportsWith) only sees `import {}
// from`, so this is the one place the "destructured a name the target never
// exports" bug - `undefined` at runtime, `TypeError` when used - is caught
// statically. Reason vocabulary mirrors ResolveNamedImportsWith: "dynamic_name_missing"
// (target exists but lacks the name) and "dynamic_target_absent" (not saved yet,
// advisory). facts supplies the export-completeness guard so a wildcard re-export
// (unknowable surface) is never flagged. `default` is always available on a
// namespace and never flagged. Deduped by (spec, name).
func ResolveDynamicNamedImports(focus string, bodies map[string]string, facts *ModuleFacts) []ImportIssue {
	body, ok := bodies[focus]
	if !ok {
		return nil
	}
	dyn := extractDynamicNamedImports(StripJSComments(body))
	if len(dyn) == 0 {
		return nil
	}
	paths := make([]string, 0, len(bodies))
	for p := range bodies {
		paths = append(paths, p)
	}
	byName, _ := ProjectModuleMap(paths)
	index := ExportIndex(bodies)

	// Per-target export surface for the actionable message (names + default).
	type exp struct {
		names  []string
		hasDef bool
	}
	cache := map[string]exp{}
	exportsOf := func(target string) exp {
		if e, ok := cache[target]; ok {
			return e
		}
		var e exp
		if tb, present := bodies[target]; present {
			code := StripJSComments(tb)
			e.names = ExtractNamedExports(code)
			e.hasDef = exportDefaultRe.MatchString(code)
		}
		cache[target] = e
		return e
	}

	var out []ImportIssue
	seen := map[string]bool{}
	for _, di := range dyn {
		if di.Imported == "default" {
			continue // the default export is always present on the namespace
		}
		key := di.Spec + "\x00" + di.Imported
		if seen[key] {
			continue
		}
		seen[key] = true

		target, present, internal := resolveImportTarget(focus, di.Spec, bodies, byName)
		if !internal {
			continue // external library: the importmap's concern, not the graph's
		}
		if !present {
			out = append(out, ImportIssue{Spec: di.Spec, Imported: di.Imported, Local: di.Local, Target: target, Reason: "dynamic_target_absent"})
			continue
		}
		if !facts.ExportsComplete(target) {
			continue // wildcard re-export: target's export set is unknowable, stay silent
		}
		exported := false
		for _, p := range index[di.Imported] {
			if p == target {
				exported = true
				break
			}
		}
		if exported {
			continue
		}
		e := exportsOf(target)
		out = append(out, ImportIssue{
			Spec: di.Spec, Imported: di.Imported, Local: di.Local, Target: target,
			Reason: "dynamic_name_missing", Exports: e.names, HasDefault: e.hasDef,
		})
	}
	return out
}

// RemovedExports returns the named exports present in oldBody but not in newBody -
// the regression set a producer-side check is bounded to, so a name that was
// never exported (a pre-existing dangling import on some sibling) is not newly
// blamed on this write. Both bodies are masked. Empty for a brand-new file.
func RemovedExports(oldBody, newBody string) []string {
	if oldBody == "" {
		return nil
	}
	newSet := map[string]bool{}
	for _, n := range ExtractNamedExports(StripJSComments(newBody)) {
		newSet[n] = true
	}
	var removed []string
	for _, n := range ExtractNamedExports(StripJSComments(oldBody)) {
		if !newSet[n] {
			removed = append(removed, n)
		}
	}
	return removed
}

// ImportedSpecFrom returns the literal module specifier the importer uses to import
// from srcPath (the first such statement in source order), or "" when the importer does
// not import from srcPath. Comments are stripped and specifiers resolve over the same
// path space the renderer uses, so an auto-wire merge targets the EXACT specifier text
// of the existing statement - adding a name to that line rather than emitting a second
// import of the same module under a differently-spelled (bare vs relative) specifier.
func ImportedSpecFrom(importerPath, srcPath string, bodies map[string]string) string {
	body := bodies[importerPath]
	if body == "" {
		return ""
	}
	paths := make([]string, 0, len(bodies))
	for p := range bodies {
		paths = append(paths, p)
	}
	byName, _ := ProjectModuleMap(paths)
	for _, ni := range ExtractNamedImports(StripJSComments(body)) {
		t, present, internal := resolveImportTarget(importerPath, ni.Spec, bodies, byName)
		if internal && present && t == srcPath {
			return ni.Spec
		}
	}
	return ""
}

// BrokenImportersAfterExportChange reports every saved module that imports a name
// in `removed` from `target` - the modules a pending write to target would break
// by dropping those exports. bodies must already carry target's NEW body so a
// re-export of the same name elsewhere is accounted for. Deterministic order.
func BrokenImportersAfterExportChange(target string, removed []string, bodies map[string]string) []ImporterBreak {
	if len(removed) == 0 {
		return nil
	}
	rem := make(map[string]bool, len(removed))
	for _, n := range removed {
		rem[n] = true
	}
	paths := make([]string, 0, len(bodies))
	for p := range bodies {
		paths = append(paths, p)
	}
	byName, _ := ProjectModuleMap(paths)

	var out []ImporterBreak
	for _, p := range paths {
		if p == target {
			continue
		}
		for _, ni := range ExtractNamedImports(StripJSComments(bodies[p])) {
			if !rem[ni.Imported] {
				continue
			}
			t, present, internal := resolveImportTarget(p, ni.Spec, bodies, byName)
			if internal && present && t == target {
				out = append(out, ImporterBreak{Importer: p, Imported: ni.Imported, Spec: ni.Spec})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Importer != out[j].Importer {
			return out[i].Importer < out[j].Importer
		}
		return out[i].Imported < out[j].Imported
	})
	return out
}
