package analysis

// Canonical LLM-facing prose for undefined-reference findings: every surface
// that reports a name a module uses but never binds (the save-time write gate
// and the two post-build static checks) steers each name through
// ExplainUndefinedRef, so the same defect reads the same wherever it is caught.
// Each surface keeps its own framing and extensions - the same shared-core /
// per-surface-extension shape as explainImportIssue. The message stays
// self-contained at runtime; only the SOURCE is single-homed.

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// firstOtherModule returns the first module in srcs that is not self, or "" - the
// module to steer a forgotten reference toward (never the file being judged).
func firstOtherModule(srcs []string, self string) string {
	for _, s := range srcs {
		if s != self {
			return s
		}
	}
	return ""
}

// ExternalBindingsElsewhere returns the namespace/default import bindings other
// modules already carry, as binding name -> specifier (`THREE` -> `three`), for
// specifiers that are NOT project modules - i.e. importmap libraries.
//
// It is the exact answer to the corpus's single most expensive defect: an entry
// writes `import * as THREE from 'three'` and a leaf then uses a bare `THREE`,
// which no import gate can see (there is no import edge) and which the page-load
// probe only catches if that code runs at boot. A sibling already naming the
// binding proves both that the specifier exists and what the import line must say,
// so the steer is a copy-paste rather than a guess. Deterministic: the first
// binding wins in sorted module order.
func ExternalBindingsElsewhere(bodies map[string]string, self string) map[string]string {
	out := map[string]string{}
	files := make([]string, 0, len(bodies))
	for p := range bodies {
		if p != self {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		src := StripJSComments(bodies[f])
		for _, m := range namespaceImportSpecRe.FindAllStringSubmatch(src, -1) {
			addExternalBinding(out, bodies, f, m[1], m[2])
		}
		for _, m := range defaultImportSpecRe.FindAllStringSubmatch(src, -1) {
			addExternalBinding(out, bodies, f, m[1], m[2])
		}
	}
	return out
}

// addExternalBinding records name -> spec when spec resolves to no project module
// (so it is an importmap library, not a sibling) and the name is not already taken.
func addExternalBinding(out map[string]string, bodies map[string]string, importer, name, spec string) {
	if name == "" || spec == "" || out[name] != "" {
		return
	}
	if _, ok := ResolveImport(importer, spec); ok {
		return // a project sibling; ExplainUndefinedRef already steers to those
	}
	if _, isProject := bodies[spec]; isProject {
		return
	}
	out[name] = spec
}

// ExplainUnboundExternalRef returns the copy-paste import line for an unbound name
// a sibling already imports from a library specifier, or "" when no sibling does.
// Callers prefer it over ExplainUndefinedRef, whose project-export lookup cannot
// resolve a library binding.
func ExplainUnboundExternalRef(r UndefinedRef, externals map[string]string) string {
	spec := externals[r.Name]
	if spec == "" || r.Kind == RefAssign {
		return ""
	}
	return fmt.Sprintf("supplied by the `%s` library, which is an importmap ES module - it is NOT an ambient global, so every file that touches it carries its own import. Add `import * as %s from '%s';` at the top of this module (a sibling already does). Never funnel it through the entry, a store module or a ctx/hooks bag: a module-level `new %s.X()` then runs before the assignment and crashes the whole file at load.",
		spec, r.Name, spec, r.Name)
}

// ExplainDeclaredExternalRef returns the import steer for an unbound name that
// case-insensitively matches a DECLARED module library's specifier or display
// name (`THREE` -> the declared `three` importmap entry). It is the first-use
// complement to ExplainUnboundExternalRef: before any sibling imports the
// library there is no binding to copy, but the declaration alone proves the
// specifier - so the FIRST file to touch the library still gets the exact
// import line instead of the generic "define it in this module" steer (the
// weak message a model then fails to generalize, repeating the slip per file).
func ExplainDeclaredExternalRef(r UndefinedRef, libs []ExternalLib) string {
	if r.Kind == RefAssign {
		return ""
	}
	for _, l := range libs {
		spec := strings.TrimSpace(l.ModuleName)
		if spec == "" || !strings.EqualFold(strings.TrimSpace(l.Kind), "module") {
			continue
		}
		if strings.EqualFold(r.Name, spec) || strings.EqualFold(r.Name, strings.TrimSpace(l.Name)) {
			return fmt.Sprintf("supplied by the declared `%s` library, an importmap ES module - it is NOT an ambient global, so every file that touches it carries its own import. Add `import * as %s from '%s';` at the top of this module. Never funnel it through the entry, a store module or a ctx/hooks bag: a module-level `new %s.X()` then runs before the assignment and crashes the whole file at load.",
				spec, r.Name, spec, r.Name)
		}
	}
	return ""
}

// ExplainUndefinedRef returns the canonical steer for ONE unbound name in file,
// resolved against the project's export and top-level-declaration indexes. The
// caller supplies the surrounding framing and the name/line prefix; this is the
// clause that must not drift between surfaces. byName (ModuleSpecifierIndex) is
// how the ready-to-paste import line gets its bare specifier.
func ExplainUndefinedRef(file string, r UndefinedRef, exportIdx, declIdx map[string][]string, byName map[string]string) string {
	owner := firstOtherModule(exportIdx[r.Name], file)

	// An import binding is read-only, so an assignment can never be steered to an
	// import the way a read can.
	if r.Kind == RefAssign {
		s := fmt.Sprintf("assigned but never declared. An ES module is always strict, so this throws - it does not create a global. Declare `let %s;` at module scope", r.Name)
		if owner != "" {
			s += fmt.Sprintf(", or - since `%s` exports `%s` and an import cannot be assigned - set it through that module's setter", path.Base(owner), r.Name)
		}
		return s + "."
	}
	if owner != "" {
		return fmt.Sprintf("exported by `%s` - add `import { %s } from '%s';`.", path.Base(owner), r.Name, SuggestImportSpecifier(file, owner, byName))
	}
	if src := firstOtherModule(declIdx[r.Name], file); src != "" {
		return fmt.Sprintf("defined but not exported in `%s` - `export` it there and `import { %s } from '%s';` here, or define it locally.", path.Base(src), r.Name, SuggestImportSpecifier(file, src, byName))
	}
	// The name resolves nowhere. A near-identical name that DOES exist usually
	// means a spelling slip of that name, so point at it before the generic steer.
	if near := nearestKnownName(r.Name, exportIdx, declIdx); near != "" {
		if owner := firstOtherModule(exportIdx[near], file); owner != "" {
			return fmt.Sprintf("no module exports or declares this name. The closest existing name is `%s`, exported by `%s` - if that is the one you meant, use `%s` (`import { %s } from '%s';`); otherwise define `%s` in this module.",
				near, path.Base(owner), near, near, SuggestImportSpecifier(file, owner, byName), r.Name)
		}
		if src := firstOtherModule(declIdx[near], file); src != "" {
			return fmt.Sprintf("no module exports or declares this name. The closest existing name is `%s`, declared (not exported) in `%s` - if that is the one you meant, `export` it there and import it here; otherwise define `%s` in this module.",
				near, path.Base(src), r.Name)
		}
		return fmt.Sprintf("no module exports or declares this name. The closest existing name is `%s`, declared in this module - if that is the one you meant, use that spelling; otherwise define `%s` here.",
			near, r.Name)
	}
	return "define it in this module, or import it from the module that should own it."
}

// nearestKnownName returns the project name closest to miss across the given
// name indexes, or "" when nothing is close enough. Deliberately conservative
// (a case slip, a dropped or added affix, or a one-character typo) so the steer
// never points at an unrelated symbol. Deterministic: the best match class
// wins, ties break lexically.
func nearestKnownName(miss string, idxs ...map[string][]string) string {
	best, bestRank := "", 0
	seen := map[string]bool{}
	for _, idx := range idxs {
		for cand := range idx {
			if cand == miss || seen[cand] {
				continue
			}
			seen[cand] = true
			r := nearNameRank(miss, cand)
			if r == 0 {
				continue
			}
			if bestRank == 0 || r < bestRank || (r == bestRank && cand < best) {
				best, bestRank = cand, r
			}
		}
	}
	return best
}

// nearNameRank scores how close cand is to miss: 1 for a case-only difference,
// 2 for an affix (one is a prefix or suffix of the other), 3 for a one-edit
// typo. 0 means not close enough to suggest.
func nearNameRank(miss, cand string) int {
	lm, lc := strings.ToLower(miss), strings.ToLower(cand)
	if lm == lc {
		return 1
	}
	shorter, longer := lm, lc
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	if len(shorter) >= 4 && (strings.HasPrefix(longer, shorter) || strings.HasSuffix(longer, shorter)) {
		return 2
	}
	if len(lm) >= 5 && withinOneEdit(lm, lc) {
		return 3
	}
	return 0
}

// withinOneEdit reports whether a and b differ by at most one character
// substitution, insertion, or deletion.
func withinOneEdit(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 1 {
		return false
	}
	i, j := 0, 0
	edited := false
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		if edited {
			return false
		}
		edited = true
		if len(a) == len(b) {
			i++
		}
		j++
	}
	return true
}

var (
	// `import * as NS from 'spec'` (optionally after a default binding), capturing
	// both the binding and the specifier.
	namespaceImportSpecRe = regexp.MustCompile(`\bimport\s+(?:[A-Za-z_$][\w$]*\s*,\s*)?\*\s+as\s+([A-Za-z_$][\w$]*)\s+from\s*['"]([^'"]+)['"]`)
	// `import Def from 'spec'` / `import Def, { a } from 'spec'`. The named-only,
	// namespace and side-effect forms have no leading identifier and never match.
	defaultImportSpecRe = regexp.MustCompile(`\bimport\s+([A-Za-z_$][\w$]*)\s*(?:,\s*(?:\{[^}]*\}|\*\s+as\s+[A-Za-z_$][\w$]*)\s*)?from\s*['"]([^'"]+)['"]`)
)
