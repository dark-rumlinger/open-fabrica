package analysis

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// Shared ES-module specifier extraction and resolution. Every consumer resolves
// an import to the exact project path the browser will request; a single resolver
// makes loads-fine-at-runtime-yet-flagged mismatches impossible.

// moduleSpecifierRe lexically matches the specifier in static/dynamic import and
// re-export statements. Exactly one capture group is non-empty per match; the
// keyword must be followed by a quoted string, so prose never matches.
var moduleSpecifierRe = regexp.MustCompile(`(?m)(?:` +
	`import\s+[^'"]*?from\s*['"]([^'"]+)['"]` + // import ... from "x"
	`|import\s*['"]([^'"]+)['"]` + // import "x"
	`|import\s*\(\s*['"]([^'"]+)['"]\s*\)` + // import("x")
	`|export\s+[^'"]*?from\s*['"]([^'"]+)['"]` + // export ... from "x"
	`)`)

// ExtractImportSpecifiers returns every module specifier referenced by a static
// or dynamic import / re-export in a JS body, in source order, duplicates
// preserved. Classification and resolution are the caller's job.
func ExtractImportSpecifiers(body string) []string {
	matches := moduleSpecifierRe.FindAllStringSubmatch(body, -1)
	if matches == nil {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		for _, g := range m[1:] {
			if g = strings.TrimSpace(g); g != "" {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// dynamicImportCallRe matches a dynamic import call and captures its raw argument
// up to the first `)`. Only a string-literal argument becomes a graph edge; a
// template/computed argument is invisible to the graph - the hole the cycle checker names.
var dynamicImportCallRe = regexp.MustCompile(`import\s*\(\s*([^)]*?)\s*\)`)

// stringLiteralArgRe matches a single plain quoted string and nothing else, the
// only dynamic-import argument shape the module graph can resolve.
var stringLiteralArgRe = regexp.MustCompile(`^(?:'[^']*'|"[^"]*")$`)

// DynamicImportArg describes one dynamic `import()` call's argument as written.
// Literal is true only when Raw is a single plain quoted string (graph-visible);
// every other shape (template literal, identifier, concatenation) is opaque to
// the static module graph.
type DynamicImportArg struct {
	Raw     string
	Literal bool
}

// ExtractDynamicImportArgs returns every dynamic `import()` call's argument in
// body, classified literal-vs-opaque. Run on a comment-stripped body so a
// commented-out import() never registers.
func ExtractDynamicImportArgs(body string) []DynamicImportArg {
	matches := dynamicImportCallRe.FindAllStringSubmatch(body, -1)
	if matches == nil {
		return nil
	}
	out := make([]DynamicImportArg, 0, len(matches))
	for _, m := range matches {
		raw := strings.TrimSpace(m[1])
		out = append(out, DynamicImportArg{Raw: raw, Literal: stringLiteralArgRe.MatchString(raw)})
	}
	return out
}

// IsRelativeSpecifier reports whether spec is a relative module specifier (./ or
// ../) - the only kind resolved against the importing file's own directory
// rather than the importmap (bare) or the web root (absolute).
func IsRelativeSpecifier(spec string) bool {
	return strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../")
}

// ResolveImport resolves a relative (./ ../) or web-absolute (/x, /files/x)
// module specifier against the importing file's project path. No extension
// completion; a trailing ?query/#fragment is stripped; ok is false for bare
// specifiers. A wrong specifier resolves faithfully ("./js/pieces.js" from
// "js/main.js" -> "js/js/pieces.js").
func ResolveImport(importerPath, spec string) (resolved string, ok bool) {
	if i := strings.IndexAny(spec, "?#"); i >= 0 {
		spec = spec[:i]
	}
	switch {
	case strings.HasPrefix(spec, "/"):
		spec = strings.TrimPrefix(spec, "/files/")
		spec = strings.TrimPrefix(spec, "/")
		return path.Clean(spec), true
	case IsRelativeSpecifier(spec):
		return path.Clean(path.Join(path.Dir(importerPath), spec)), true
	default:
		return "", false
	}
}

// ModuleGraph is the ES-module import graph over a set of JS artifact bodies.
// Edges maps importer path to its deduped, source-order imports; Nodes is sorted
// for deterministic traversal. Relative and internal-bare specifiers that resolve
// into the input set become edges; dangling imports are dropped (the unresolved-import
// checker's concern). Self-edges are kept so a self-import is observable as a cycle.
type ModuleGraph struct {
	Edges map[string][]string
	Nodes []string
}

// BuildModuleGraph builds the import graph for bodies (project path -> JS
// source). Scope-agnostic; callers may pre-strip comments. Resolution mirrors
// the runtime server and the readiness validator.
//
// This is one of two import-graph builders, kept separate on purpose (do NOT
// "dedupe" them into one - it changes a verdict). This regex builder is the
// RESILIENT reachability graph: it reads imports even from a module that does not
// parse. The esbuild builder (AnalyzeModules -> ModuleFacts.Edges) is the
// resolution-correctness graph for cycles and binding faults; the two diverge
// only on unparseable input (pinned by TestImportGraphs_AgreeOnWellFormed and
// TestImportGraphs_DivergeOnUnparseableInput).
func BuildModuleGraph(bodies map[string]string) ModuleGraph {
	g := ModuleGraph{Edges: make(map[string][]string), Nodes: make([]string, 0, len(bodies))}
	for p := range bodies {
		g.Nodes = append(g.Nodes, p)
	}
	sort.Strings(g.Nodes)
	// Logical-name index so a bare specifier that names an internal module
	// ("world-gen") becomes an edge, exactly like its relative form
	// ("./world-gen.js"). An external bare specifier (e.g. "three", a CDN
	// importmap entry) doesn't match a project module and stays a non-edge.
	byName, _ := ProjectModuleMap(g.Nodes)
	for _, importer := range g.Nodes {
		seen := make(map[string]bool)
		for _, spec := range ExtractImportSpecifiers(bodies[importer]) {
			var dep string
			if IsRelativeSpecifier(spec) {
				base, ok := ResolveImport(importer, spec)
				if !ok {
					continue
				}
				dep = resolveInSet(base, bodies)
			} else if internal, ok := ResolveProjectModuleSpecifier(spec, byName); ok {
				dep = internal
			}
			if dep == "" || seen[dep] {
				continue
			}
			seen[dep] = true
			g.Edges[importer] = append(g.Edges[importer], dep)
		}
	}
	return g
}

// resolveInSet returns the member of bodies that base resolves to - the exact
// path, or (when base has no extension) an extension/index completion - or ""
// when none match.
func resolveInSet(base string, bodies map[string]string) string {
	if _, ok := bodies[base]; ok {
		return base
	}
	if path.Ext(base) == "" {
		for _, c := range []string{base + ".js", base + ".mjs", path.Join(base, "index.js")} {
			if _, ok := bodies[c]; ok {
				return c
			}
		}
	}
	return ""
}

// exportFuncClassRe matches a function/class export, which binds exactly one
// name: `export function foo`, `export async function* gen`, `export class Bar`.
var exportFuncClassRe = regexp.MustCompile(`(?m)\bexport\s+(?:async\s+)?(?:function\s*\*?|class)\s+([A-Za-z_$][\w$]*)`)

// exportVarRe matches the START of a variable export (`export const|let|var`);
// appendDeclaratorNames then captures EVERY binding. A regex capturing only the
// first identifier would silently drop the rest, false-rejecting a later dynamic
// import of the missed name.
var exportVarRe = regexp.MustCompile(`(?m)\bexport\s+(?:const|let|var)\s+`)

// exportListRe matches a brace export list: `export { a, b as c }` and the
// re-export form `export { a, b } from './m.js'`. The capture is the raw list.
var exportListRe = regexp.MustCompile(`\bexport\s*\{([^}]*)\}`)

// namedImportRe matches a named import (optionally alongside a default binding):
// `import { a, b as c } from 'spec'` and `import Def, { a } from 'spec'`. Group 1
// is the brace list, group 2 the specifier. Namespace (`import * as`) and
// default-only imports impose no named-export requirement and are not matched.
var namedImportRe = regexp.MustCompile(`(?m)\bimport\s+(?:[A-Za-z_$][\w$]*\s*,\s*)?\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)

// ExtractNamedExports returns the distinct top-level names a module exports via a
// named declaration or a brace list (re-exports included). The default export is
// ignored (it is not a named export). Run on a comment-stripped body for best
// results. Order is source order with duplicates removed.
func ExtractNamedExports(body string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || name == "default" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, m := range exportFuncClassRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, loc := range exportVarRe.FindAllStringIndex(body, -1) {
		appendDeclaratorNames(add, body[loc[1]:])
	}
	for _, m := range exportListRe.FindAllStringSubmatch(body, -1) {
		for _, tok := range strings.Split(m[1], ",") {
			// `x as y` exports y; `default as y` exports y; plain `x` exports x.
			if f := strings.Fields(tok); len(f) >= 3 && f[1] == "as" {
				add(f[2])
			} else {
				add(strings.TrimSpace(tok))
			}
		}
	}
	return out
}

// appendDeclaratorNames extracts every bound name from a const/let/var declarator
// list. It reads each binding target, then skips the initializer (tracking (), [],
// {} and string/template nesting) so an initializer comma never splits a declarator.
func appendDeclaratorNames(add func(string), s string) {
	for i, n := 0, len(s); i < n; {
		for i < n && isSpaceByte(s[i]) {
			i++
		}
		if i >= n {
			return
		}
		switch c := s[i]; {
		case c == '{' || c == '[':
			j := skipBalancedJS(s, i)
			appendPatternNames(add, s[i:j])
			i = j
		case isIdentStartByte(c):
			k := i + 1
			for k < n && isIdentByte(s[k]) {
				k++
			}
			add(s[i:k])
			i = k
		default:
			return // not a binding we understand; stop reading this declaration
		}
		i = nextDeclarator(s, i)
	}
}

// appendPatternNames extracts the identifiers a destructuring pattern binds.
// Defaults (`= expr`) are dropped, rest and nested patterns recurse; initializer
// expressions are never mined for names, so `{ a = foo }` binds only `a`.
func appendPatternNames(add func(string), pat string) {
	if len(pat) < 2 {
		return
	}
	object := pat[0] == '{'
	for _, el := range splitTopLevelCommas(pat[1 : len(pat)-1]) {
		el = strings.TrimSpace(beforeTopLevelAssign(el))
		if el == "" {
			continue // array hole, rest with no name, or trailing comma
		}
		if strings.HasPrefix(el, "...") {
			el = strings.TrimSpace(el[3:])
		}
		if object {
			if idx := topLevelColon(el); idx >= 0 {
				el = strings.TrimSpace(el[idx+1:]) // `key: target` binds target
			}
		}
		switch {
		case el == "":
			continue
		case el[0] == '{' || el[0] == '[':
			appendPatternNames(add, el[:skipBalancedJS(el, 0)]) // nested pattern
		case isIdentStartByte(el[0]):
			k := 1
			for k < len(el) && isIdentByte(el[k]) {
				k++
			}
			add(el[:k])
		}
	}
}

// nextDeclarator returns the index just past the next top-level comma in a
// declarator list, or len(s) when the declaration ends (a top-level `;`, a
// top-level line break - ASI - or a closing bracket we did not open). Strings
// and bracket nesting are tracked so a comma inside an initializer is ignored.
func nextDeclarator(s string, i int) int {
	depth := 0
	for n := len(s); i < n; i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			i = skipStringJS(s, i) - 1
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return n
			}
			depth--
		case ',':
			if depth == 0 {
				return i + 1
			}
		case ';', '\n', '\r':
			if depth == 0 {
				return n
			}
		}
	}
	return len(s)
}

// splitTopLevelCommas splits s on commas that sit outside any bracket nesting and
// outside string/template literals.
func splitTopLevelCommas(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, n := 0, len(s); i < n; i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			i = skipStringJS(s, i) - 1
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// beforeTopLevelAssign returns the prefix of s before its first top-level `=`
// (a destructuring default), excluding `==`, `===`, `=>`, `<=`, `>=`, `!=`.
// Returns s unchanged when there is no such assignment.
func beforeTopLevelAssign(s string) string {
	depth := 0
	for i, n := 0, len(s); i < n; i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			i = skipStringJS(s, i) - 1
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth != 0 {
				continue
			}
			var next, prev byte
			if i+1 < n {
				next = s[i+1]
			}
			if i > 0 {
				prev = s[i-1]
			}
			if next != '=' && next != '>' && prev != '=' && prev != '!' && prev != '<' && prev != '>' {
				return s[:i]
			}
		}
	}
	return s
}

// topLevelColon returns the index of the first `:` outside any bracket nesting
// and outside a string, or -1. Used to split an object-pattern `key: target`.
func topLevelColon(s string) int {
	depth := 0
	for i, n := 0, len(s); i < n; i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			i = skipStringJS(s, i) - 1
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// skipBalancedJS returns the index just past the bracket that matches the opener
// at s[i] (which must be one of ( [ {), tracking nested brackets and skipping
// string/template literals. Returns len(s) when unbalanced.
func skipBalancedJS(s string, i int) int {
	depth := 0
	for n := len(s); i < n; i++ {
		switch c := s[i]; c {
		case '"', '\'', '`':
			i = skipStringJS(s, i) - 1
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}

// skipStringJS returns the index just past the string/template literal that opens
// at s[i] (a quote or backtick), honouring backslash escapes. Template
// interpolation is treated as opaque (scanned to the next unescaped backtick),
// which is sufficient for splitting declarators. Returns len(s) when unterminated.
func skipStringJS(s string, i int) int {
	q := s[i]
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case q:
			return i + 1
		}
	}
	return len(s)
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
}

func isIdentStartByte(b byte) bool {
	return b == '_' || b == '$' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// NamedImport is one name pulled in by a named import: Spec is the module
// specifier, Imported is the name the target module must export (the pre-`as`
// name), and Local is the binding introduced in the importer (the post-`as`
// name, equal to Imported when there is no alias).
type NamedImport struct {
	Spec     string
	Imported string
	Local    string
}

// ExtractNamedImports returns every named import in body. Run on a
// comment-stripped body for best results.
func ExtractNamedImports(body string) []NamedImport {
	var out []NamedImport
	for _, m := range namedImportRe.FindAllStringSubmatch(body, -1) {
		spec := m[2]
		for _, tok := range strings.Split(m[1], ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			imported, local := tok, tok
			if f := strings.Fields(tok); len(f) >= 3 && f[1] == "as" {
				imported, local = f[0], f[2]
			}
			out = append(out, NamedImport{Spec: spec, Imported: imported, Local: local})
		}
	}
	return out
}

// ImportRelocation moves a single named import from one module specifier to
// another. Imported is the pre-`as` name; FromSpec is the (wrong) specifier it is
// currently imported from; ToSpec is the correct specifier to import it from.
type ImportRelocation struct {
	Imported string
	FromSpec string
	ToSpec   string
}

// RelocateNamedImports rewrites body so each relocation's Imported name is
// imported from ToSpec instead of FromSpec, merging into an existing ToSpec
// statement or prepending a fresh one. Only the brace contents of a touched
// statement change, so default-import prefixes and specifier text are preserved
// verbatim. Written for plain (non-aliased) relocated names only.
func RelocateNamedImports(body string, relocations []ImportRelocation) string {
	if len(relocations) == 0 {
		return body
	}
	// FromSpec -> (name -> ToSpec).
	moves := map[string]map[string]string{}
	for _, r := range relocations {
		if moves[r.FromSpec] == nil {
			moves[r.FromSpec] = map[string]string{}
		}
		moves[r.FromSpec][r.Imported] = r.ToSpec
	}
	// keepTokens rebuilds a brace name-list, calling visit(preAsName) per token and
	// keeping the token verbatim (aliases and all) when visit returns keep=true.
	keepTokens := func(namesText string, visit func(name string) (keep bool)) []string {
		var kept []string
		for _, tok := range strings.Split(namesText, ",") {
			t := strings.TrimSpace(tok)
			if t == "" {
				continue
			}
			name := t
			if f := strings.Fields(t); len(f) >= 1 {
				name = f[0] // pre-`as` name
			}
			if visit(name) {
				kept = append(kept, t)
			}
		}
		return kept
	}
	// Pass 1: drop each moved name from its SOURCE statement, recording ONLY the
	// names actually removed (grouped by target) - so a relocation whose source name
	// isn't present adds nothing (a true move, never a stray import).
	addBySpec := map[string]map[string]bool{}
	out := namedImportRe.ReplaceAllStringFunc(body, func(stmt string) string {
		m := namedImportRe.FindStringSubmatch(stmt)
		if m == nil {
			return stmt
		}
		namesText, spec := m[1], m[2]
		rem := moves[spec]
		if len(rem) == 0 {
			return stmt
		}
		kept := keepTokens(namesText, func(name string) bool {
			to, ok := rem[name]
			if ok {
				if addBySpec[to] == nil {
					addBySpec[to] = map[string]bool{}
				}
				addBySpec[to][name] = true
			}
			return !ok
		})
		if len(kept) == 0 {
			return "" // statement now empty: drop it (a bare `;` may remain, harmless)
		}
		return strings.Replace(stmt, "{"+namesText+"}", "{ "+strings.Join(kept, ", ")+" }", 1)
	})
	if len(addBySpec) == 0 {
		return out
	}
	// Pass 2: merge each target's moved names into an existing statement for it (the
	// first one), else prepend a fresh line. Names already present are not duplicated.
	merged := map[string]bool{}
	out = namedImportRe.ReplaceAllStringFunc(out, func(stmt string) string {
		m := namedImportRe.FindStringSubmatch(stmt)
		if m == nil {
			return stmt
		}
		namesText, spec := m[1], m[2]
		add := addBySpec[spec]
		if len(add) == 0 || merged[spec] {
			return stmt
		}
		merged[spec] = true
		present := map[string]bool{}
		toks := keepTokens(namesText, func(name string) bool { present[name] = true; return true })
		extra := sortedMissing(add, present)
		if len(extra) == 0 {
			return stmt
		}
		return strings.Replace(stmt, "{"+namesText+"}", "{ "+strings.Join(append(toks, extra...), ", ")+" }", 1)
	})
	specs := make([]string, 0, len(addBySpec))
	for s := range addBySpec {
		specs = append(specs, s)
	}
	sort.Strings(specs)
	var head strings.Builder
	for _, s := range specs {
		if merged[s] {
			continue
		}
		head.WriteString("import { " + strings.Join(sortedMissing(addBySpec[s], nil), ", ") + " } from '" + s + "';\n")
	}
	return head.String() + out
}

// sortedMissing returns the names in set that are not in present, sorted.
func sortedMissing(set, present map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		if !present[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// AddNamedImports merges names into body's import graph: each name joins the
// first existing `import { ... } from '<spec>'` statement, else a fresh line is
// prepended. Existing names, aliases, and default-import prefixes are preserved
// verbatim. Deterministic: added names and fresh specs are sorted.
func AddNamedImports(body string, additions map[string][]string) string {
	if len(additions) == 0 {
		return body
	}
	addBySpec := map[string]map[string]bool{}
	for spec, names := range additions {
		for _, n := range names {
			if n == "" {
				continue
			}
			if addBySpec[spec] == nil {
				addBySpec[spec] = map[string]bool{}
			}
			addBySpec[spec][n] = true
		}
	}
	if len(addBySpec) == 0 {
		return body
	}
	// Merge into the first existing statement per spec.
	merged := map[string]bool{}
	out := namedImportRe.ReplaceAllStringFunc(body, func(stmt string) string {
		m := namedImportRe.FindStringSubmatch(stmt)
		if m == nil {
			return stmt
		}
		namesText, spec := m[1], m[2]
		add := addBySpec[spec]
		if len(add) == 0 || merged[spec] {
			return stmt
		}
		merged[spec] = true
		present := map[string]bool{}
		var toks []string
		for _, tok := range strings.Split(namesText, ",") {
			t := strings.TrimSpace(tok)
			if t == "" {
				continue
			}
			name := t
			if f := strings.Fields(t); len(f) >= 1 {
				name = f[0] // pre-`as` name
			}
			present[name] = true
			toks = append(toks, t)
		}
		extra := sortedMissing(add, present)
		if len(extra) == 0 {
			return stmt
		}
		return strings.Replace(stmt, "{"+namesText+"}", "{ "+strings.Join(append(toks, extra...), ", ")+" }", 1)
	})
	// Any spec with no existing statement: prepend a fresh line, sorted for determinism.
	specs := make([]string, 0, len(addBySpec))
	for s := range addBySpec {
		specs = append(specs, s)
	}
	sort.Strings(specs)
	var head strings.Builder
	for _, s := range specs {
		if merged[s] {
			continue
		}
		head.WriteString("import { " + strings.Join(sortedMissing(addBySpec[s], nil), ", ") + " } from '" + s + "';\n")
	}
	return head.String() + out
}

// DedupeNamedImports removes redundant duplicate named imports (SAME name, SAME
// specifier, SAME local binding), collapsing to the FIRST occurrence - a
// semantics-free fix for a hard "already declared" error. Only EXACT duplicates
// are removed: a different specifier or alias is a distinct binding (a real
// conflict) and both are left in place. Returns the body and a label per removed import.
func DedupeNamedImports(body string) (string, []string) {
	seen := map[string]bool{}
	var removed []string
	out := namedImportRe.ReplaceAllStringFunc(body, func(stmt string) string {
		m := namedImportRe.FindStringSubmatch(stmt)
		if m == nil {
			return stmt
		}
		namesText, spec := m[1], m[2]
		var kept []string
		changed := false
		for _, tok := range strings.Split(namesText, ",") {
			t := strings.TrimSpace(tok)
			if t == "" {
				continue // empty token (trailing comma): drop without flagging a change
			}
			imported, local := t, t
			if f := strings.Fields(t); len(f) >= 3 && f[1] == "as" {
				imported, local = f[0], f[2]
			}
			key := spec + "\x00" + imported + "\x00" + local
			if seen[key] {
				removed = append(removed, "removed duplicate import { "+t+" } from '"+spec+"'")
				changed = true
				continue
			}
			seen[key] = true
			kept = append(kept, t)
		}
		if !changed {
			return stmt // nothing removed from this statement: leave it verbatim
		}
		if len(kept) == 0 {
			return "" // the whole statement was duplicate: drop it (a bare `;` may remain, harmless)
		}
		return strings.Replace(stmt, "{"+namesText+"}", "{ "+strings.Join(kept, ", ")+" }", 1)
	})
	return out, removed
}

// DynamicNamedImport is one name pulled in by destructuring a LITERAL dynamic
// import - `import('x').then(({ a, b: c }) => ...)` or `const { a } = await
// import('x')`. Spec is the literal module specifier, Imported the name the target
// must export (the pre-`:` name), Local the binding it introduces.
type DynamicNamedImport struct {
	Spec     string
	Imported string
	Local    string
}

var (
	// `import('x').then(({ ... }) => ...)`: the destructured param of the callback.
	// An arrow with a destructuring param always parenthesizes it, so `(\s*\{` is
	// the reliable anchor.
	dynThenDestructureRe = regexp.MustCompile(`import\s*\(\s*['"]([^'"]+)['"]\s*\)\s*\.\s*then\s*\(\s*(?:async\s+)?\(\s*\{([^}]*)\}`)
	// `const|let|var { ... } = await import('x')`.
	dynAwaitDestructureRe = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]*)\}\s*=\s*await\s+import\s*\(\s*['"]([^'"]+)['"]\s*\)`)
	dynBindingIdentRe     = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
)

// extractDynamicNamedImports returns every name destructured from a LITERAL
// dynamic import in body. Opaque imports, namespace access, and rest/computed/
// nested elements are intentionally NOT reported - stay silent rather than guess.
// Run on a comment-stripped body so a commented-out import never registers.
func extractDynamicNamedImports(body string) []DynamicNamedImport {
	var out []DynamicNamedImport
	emit := func(spec, list string) {
		for _, raw := range strings.Split(list, ",") {
			tok := strings.TrimSpace(raw)
			if tok == "" {
				continue
			}
			if i := strings.IndexByte(tok, '='); i >= 0 { // drop a default value (`a = expr`)
				tok = strings.TrimSpace(tok[:i])
			}
			imported, local := tok, tok
			if i := strings.IndexByte(tok, ':'); i >= 0 { // `imported: local`
				imported = strings.TrimSpace(tok[:i])
				local = strings.TrimSpace(tok[i+1:])
			}
			if !dynBindingIdentRe.MatchString(imported) {
				continue // rest (...x), computed ([k]), nested ({..}): skip, do not guess
			}
			out = append(out, DynamicNamedImport{Spec: spec, Imported: imported, Local: local})
		}
	}
	for _, m := range dynThenDestructureRe.FindAllStringSubmatch(body, -1) {
		emit(m[1], m[2])
	}
	for _, m := range dynAwaitDestructureRe.FindAllStringSubmatch(body, -1) {
		emit(m[2], m[1])
	}
	return out
}

// UnresolvedSpecifier is one BARE import specifier in a module that resolves to
// neither a project module nor a declared importmap library - so the browser
// throws `Failed to resolve module specifier "<Spec>"` at load.
type UnresolvedSpecifier struct {
	File string
	Spec string
	// Collides is true when the specifier is unresolvable because TWO project
	// modules share this basename, not because nothing provides it. The two cases
	// need opposite fixes - declare a library vs. rename one of your own files - so
	// the distinction is made here, beside ProjectModuleMap which computes it, and
	// never re-derived by a consumer.
	Collides bool
}

// UnresolvedModuleSpecifiers scans every module in bodies for BARE import
// specifiers that resolve to NEITHER a project module NOR an importmap entry in
// libSpecs (a libSpec ending in "/" is a subpath PREFIX). This is the static
// catch for the runtime "Failed to resolve module specifier" that esbuild and the
// page probe both miss. Deterministic: files sorted, source order, deduped per (file, spec).
func UnresolvedModuleSpecifiers(bodies map[string]string, libSpecs []string) []UnresolvedSpecifier {
	exact := make(map[string]bool, len(libSpecs))
	var prefixes []string
	for _, s := range libSpecs {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if strings.HasSuffix(s, "/") {
			prefixes = append(prefixes, s)
		} else {
			exact[s] = true
		}
	}
	paths := make([]string, 0, len(bodies))
	for p := range bodies {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	byName, collisions := ProjectModuleMap(paths)
	colliding := make(map[string]bool, len(collisions))
	for _, c := range collisions {
		colliding[c] = true
	}

	covered := func(spec string) bool {
		if exact[spec] {
			return true
		}
		for _, pre := range prefixes {
			if strings.HasPrefix(spec, pre) {
				return true
			}
		}
		_, ok := ResolveProjectModuleSpecifier(spec, byName)
		return ok
	}

	var out []UnresolvedSpecifier
	for _, p := range paths {
		seen := map[string]bool{}
		for _, spec := range ExtractImportSpecifiers(StripJSComments(bodies[p])) {
			// Only bare specifiers: relative (./ ../) and web-absolute (/...) resolve
			// through the file graph, checked by the module_imports / link gates.
			if IsRelativeSpecifier(spec) || strings.HasPrefix(spec, "/") {
				continue
			}
			if seen[spec] {
				continue
			}
			seen[spec] = true
			if !covered(spec) {
				out = append(out, UnresolvedSpecifier{File: p, Spec: spec, Collides: colliding[spec]})
			}
		}
	}
	return out
}
