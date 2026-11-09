package analysis

// Relative -> bare specifier normalization. A relative specifier resolves to the
// UNVERSIONED URL - a different browser module-map key - so mixing it with the
// bare form instantiates the module twice and forks every live-binding singleton.
// The save gate rewrites relative project-module specifiers to the bare form.

import (
	"strings"
)

// SpecifierRewrite records one applied rewrite, for the save echo.
type SpecifierRewrite struct {
	From string // the relative specifier as written
	To   string // the bare logical name it became
}

// RewriteRelativeSpecifiers rewrites every relative (./ ../) static-import,
// re-export, and literal dynamic-import specifier whose resolved target byName
// maps to an unambiguous bare logical name; an unmappable specifier stays as
// written. Purely textual on the quoted specifier: the statement around it is
// preserved byte-for-byte.
func RewriteRelativeSpecifiers(importerPath, body string, byName map[string]string) (string, []SpecifierRewrite) {
	var rewrites []SpecifierRewrite
	locs := moduleSpecifierRe.FindAllStringSubmatchIndex(body, -1)
	if len(locs) == 0 {
		return body, nil
	}
	code := jsCodeMask(body)
	var b strings.Builder
	prev := 0
	for _, loc := range locs {
		// A statement-looking run inside a string, template or comment is prose or data,
		// not an import: rewriting it would corrupt the text.
		if !code[loc[0]] {
			continue
		}
		spec, specStart, specEnd := "", -1, -1
		for g := 1; g*2+1 < len(loc); g++ {
			if loc[g*2] >= 0 && strings.TrimSpace(body[loc[g*2]:loc[g*2+1]]) != "" {
				spec, specStart, specEnd = strings.TrimSpace(body[loc[g*2]:loc[g*2+1]]), loc[g*2], loc[g*2+1]
				break
			}
		}
		if !IsRelativeSpecifier(spec) {
			continue
		}
		target, ok := ResolveImport(importerPath, spec)
		if !ok {
			continue
		}
		name := ProjectModuleName(target)
		if name == "" || byName[name] != target {
			continue
		}
		// Replace exactly the specifier text between its quotes; the quote characters
		// around it are rewritten to single quotes.
		if specStart < 1 || specEnd >= len(body) || specStart < loc[0] || specEnd > loc[1] {
			continue
		}
		rewrites = append(rewrites, SpecifierRewrite{From: spec, To: name})
		b.WriteString(body[prev : specStart-1])
		b.WriteString("'" + name + "'")
		prev = specEnd + 1
	}
	if len(rewrites) == 0 {
		return body, nil
	}
	b.WriteString(body[prev:])
	return b.String(), rewrites
}

// jsCodeMask marks, per byte of a JS body, whether it is code: false inside comments,
// quoted strings and template literals. It is a lexical pass (no regex-literal
// awareness), enough to keep text-level rewrites out of string data and comments.
func jsCodeMask(s string) []bool {
	mask := make([]bool, len(s))
	const (
		code = iota
		lineComment
		blockComment
		single
		double
		template
	)
	state := code
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(s) && s[i+1] == '/':
				state = lineComment
			case c == '/' && i+1 < len(s) && s[i+1] == '*':
				state = blockComment
			case c == '\'':
				state = single
			case c == '"':
				state = double
			case c == '`':
				state = template
			default:
				mask[i] = true
			}
			// The opening quote or slash is itself not code; specifier quotes are only
			// consulted via the match start, which is the `import` / `export` keyword.
		case lineComment:
			if c == '\n' {
				state = code
				mask[i] = true
			}
		case blockComment:
			if c == '*' && i+1 < len(s) && s[i+1] == '/' {
				i++
				state = code
			}
		case single, double, template:
			switch {
			case c == '\\':
				i++
			case state == single && c == '\'', state == double && c == '"', state == template && c == '`':
				state = code
			case c == '\n' && state != template:
				state = code // an unterminated string ends at the line break
			}
		}
	}
	return mask
}
