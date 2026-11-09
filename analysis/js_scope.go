package analysis

// Lexical scope for the undefined-reference analysis: which names a given byte
// offset in a module can see. A binding region is a function's parameter list
// plus body, a catch clause, or a class body; regions nest, and a name resolves
// when module scope or any containing region binds it.
//
// Two deliberate over-collections keep the analysis suppress-only (each can
// hide a finding, never invent one): block scope is folded into the surrounding
// region, and a region binds every name declared anywhere inside it, nested
// regions included. `var` hoisting needs no handling of its own: a region
// already spans the whole function, which is exactly a `var`'s scope.

import (
	"strings"
)

// jsScope is one binding region: the half-open byte range [start,end) it covers
// and the names bound within it.
type jsScope struct {
	start, end int
	names      map[string]bool
}

// jsScopes is a module's scope tree, flattened. Module holds the names visible
// everywhere (imports and every declaration outside any region); inner holds one
// entry per binding region.
type jsScopes struct {
	module map[string]bool
	inner  []jsScope
}

// resolves reports whether name is bound at byte offset off - module scope, or
// any region covering off.
func (s *jsScopes) resolves(name string, off int) bool {
	if s.module[name] {
		return true
	}
	for i := range s.inner {
		if off >= s.inner[i].start && off < s.inner[i].end && s.inner[i].names[name] {
			return true
		}
	}
	return false
}

// analyzeScopes builds the scope tree for a module body. masked must be maskJS
// output, so a name inside a string or comment never registers as a binding.
func analyzeScopes(masked string) *jsScopes {
	regions := jsRegions(masked)
	s := &jsScopes{module: moduleScopeNames(masked, regions), inner: regions}
	return s
}

// moduleScopeNames collects the names visible across the whole module: every
// imported local, plus every declaration outside every region. The test is
// "outside every region" rather than "at bracket depth zero" so that a top-level
// block or `for` head, whose declarations are module-scope here, still binds.
func moduleScopeNames(masked string, regions []jsScope) map[string]bool {
	d := map[string]bool{}
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			d[n] = true
		}
	}
	addImportedLocals(masked, add)
	inRegion := func(off int) bool {
		for i := range regions {
			if off >= regions[i].start && off < regions[i].end {
				return true
			}
		}
		return false
	}
	for _, loc := range varDeclKwRe.FindAllStringIndex(masked, -1) {
		if !inRegion(loc[0]) {
			appendDeclaratorNames(add, masked[loc[1]:])
		}
	}
	for _, m := range funcDeclRe.FindAllStringSubmatchIndex(masked, -1) {
		if !inRegion(m[0]) && m[2] >= 0 {
			add(masked[m[2]:m[3]])
		}
	}
	for _, m := range classDeclRe.FindAllStringSubmatchIndex(masked, -1) {
		if !inRegion(m[0]) && m[2] >= 0 {
			add(masked[m[2]:m[3]])
		}
	}
	return d
}

// addImportedLocals adds the local binding each import statement introduces:
// named (`import { a, b as c }`), default (`import d`), and namespace
// (`import * as ns`).
func addImportedLocals(masked string, add func(string)) {
	for _, ni := range ExtractNamedImports(masked) {
		add(ni.Local)
	}
	for _, m := range defaultImportRe.FindAllStringSubmatch(masked, -1) {
		add(m[1])
	}
	for _, m := range namespaceImportRe.FindAllStringSubmatch(masked, -1) {
		add(m[1])
	}
}

// jsRegions finds every binding region in masked: function parameter-list+body
// pairs, arrows (both block- and expression-bodied), catch clauses, and class
// bodies. A region's range starts at its parameter list, so a default value
// resolves against the region that binds it.
func jsRegions(masked string) []jsScope {
	var out []jsScope
	n := len(masked)
	for i := 0; i < n; i++ {
		switch {
		case masked[i] == '=' && i+1 < n && masked[i+1] == '>':
			if r, ok := arrowRegion(masked, i); ok {
				out = append(out, r)
			}
		case isIdentStartByte(masked[i]) && !identContinues(masked, i):
			j := i
			for j < n && isIdentByte(masked[j]) {
				j++
			}
			word := masked[i:j]
			switch word {
			case "function":
				if r, ok := headerRegion(masked, j); ok {
					out = append(out, r)
				}
			case "catch":
				if r, ok := headerRegion(masked, j); ok {
					out = append(out, r)
				}
			case "class":
				if r, ok := classRegion(masked, j); ok {
					out = append(out, r)
				}
			default:
				if jsKeywords[word] {
					break
				}
				// A method / getter / setter head: `name(params) {`. Only a `{`
				// directly after the parameter list makes it a definition, so a
				// plain call never reaches here.
				if r, ok := headerRegion(masked, j); ok {
					out = append(out, r)
				}
			}
			i = j - 1
		}
	}
	return out
}

// identContinues reports whether the byte before i continues an identifier or is
// a `.`, i.e. i is not the start of a standalone word.
func identContinues(masked string, i int) bool {
	if i == 0 {
		return false
	}
	c := masked[i-1]
	return c == '.' || isIdentByte(c)
}

// headerRegion reads a `(params) {body}` pair starting at or after from, and
// returns the region spanning both. ok is false when no parameter list or no
// block body directly follows - the shape is then a call, not a definition.
func headerRegion(masked string, from int) (jsScope, bool) {
	open := skipSpaceAnd(masked, from, "*")
	if open >= len(masked) || masked[open] != '(' {
		return jsScope{}, false
	}
	closeIdx := skipBalancedJS(masked, open)
	brace := skipSpaceAnd(masked, closeIdx, "")
	if brace >= len(masked) || masked[brace] != '{' {
		return jsScope{}, false
	}
	end := skipBalancedJS(masked, brace)
	names := map[string]bool{}
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			names[n] = true
		}
	}
	appendPatternNames(add, "["+masked[open+1:closeIdx-1]+"]")
	addRegionDeclarations(masked[brace:end], add)
	return jsScope{start: open, end: end, names: names}, true
}

// arrowRegion reads the arrow whose `=>` sits at arrow, returning the region its
// parameters bind over: the parameter list plus a block body, or plus the
// expression the arrow returns.
func arrowRegion(masked string, arrow int) (jsScope, bool) {
	start, params, ok := arrowParams(masked, arrow)
	if !ok {
		return jsScope{}, false
	}
	body := skipSpaceAnd(masked, arrow+2, "")
	var end int
	if body < len(masked) && masked[body] == '{' {
		end = skipBalancedJS(masked, body)
	} else {
		end = arrowExprEnd(masked, body)
	}
	names := map[string]bool{}
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			names[n] = true
		}
	}
	appendPatternNames(add, "["+params+"]")
	addRegionDeclarations(masked[body:end], add)
	return jsScope{start: start, end: end, names: names}, true
}

// arrowParams reads the parameter text of the arrow whose `=>` sits at arrow,
// walking back over either a balanced `(...)` list or a single bare identifier.
// The returned offset is where that list starts.
func arrowParams(masked string, arrow int) (start int, params string, ok bool) {
	i := arrow - 1
	for i >= 0 && isSpaceByte(masked[i]) {
		i--
	}
	if i < 0 {
		return 0, "", false
	}
	if masked[i] == ')' {
		open := matchOpenParen(masked, i)
		if open < 0 {
			return 0, "", false
		}
		return open, masked[open+1 : i], true
	}
	if !isIdentByte(masked[i]) {
		return 0, "", false
	}
	j := i + 1
	for i >= 0 && isIdentByte(masked[i]) {
		i--
	}
	name := masked[i+1 : j]
	if jsKeywords[name] {
		return 0, "", false // `async x => ...` is handled by the ident before it
	}
	return i + 1, name, true
}

// matchOpenParen returns the index of the `(` matching the `)` at close, or -1
// when unbalanced. Strings are already blanked by maskJS, so a paren inside one
// cannot mismatch here.
func matchOpenParen(masked string, close int) int {
	depth := 0
	for i := close; i >= 0; i-- {
		switch masked[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// arrowExprEnd returns the end of an expression-bodied arrow's body: the first
// top-level `,` or `;`, or the closer of a bracket the expression did not open.
func arrowExprEnd(masked string, from int) int {
	depth := 0
	for i := from; i < len(masked); i++ {
		switch c := masked[i]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return i
			}
			depth--
		case ',', ';':
			if depth == 0 {
				return i
			}
		}
	}
	return len(masked)
}

// classRegion reads the body of the class whose `class` keyword ends at from,
// binding its field and method names.
func classRegion(masked string, from int) (jsScope, bool) {
	brace := strings.IndexByte(masked[from:], '{')
	if brace < 0 {
		return jsScope{}, false
	}
	brace += from
	end := skipBalancedJS(masked, brace)
	names := map[string]bool{}
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			names[n] = true
		}
	}
	addClassMemberNames(masked[brace:end], add)
	return jsScope{start: brace, end: end, names: names}, true
}

// addClassMemberNames adds a class body's own member names - the fields and
// methods declared directly in it - so a field initializer (`count = 0`) never
// reads as an assignment to an undeclared name.
func addClassMemberNames(body string, add func(string)) {
	depth := 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '{' || c == '(' || c == '[':
			depth++
			continue
		case c == '}' || c == ')' || c == ']':
			depth--
			continue
		case depth != 1 || !isIdentStartByte(c) || identContinues(body, i):
			continue
		}
		j := i
		for j < len(body) && isIdentByte(body[j]) {
			j++
		}
		k := skipSpaceAnd(body, j, "")
		if k < len(body) && (body[k] == '=' || body[k] == '(') {
			add(body[i:j])
		}
		i = j - 1
	}
}

// addRegionDeclarations adds every name a region's body binds: var/let/const
// declarators (destructuring included), function and class declarations, and the
// parameters of any nested function, arrow, or catch. Nested bindings are
// included per the over-collection rule at the top of the file.
func addRegionDeclarations(body string, add func(string)) {
	for _, loc := range varDeclKwRe.FindAllStringIndex(body, -1) {
		appendDeclaratorNames(add, body[loc[1]:])
	}
	for _, m := range funcDeclRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range classDeclRe.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	collectParams(body, add)
}

// skipSpaceAnd returns the index of the first byte at or after i that is neither
// whitespace nor one of the bytes in also.
func skipSpaceAnd(s string, i int, also string) int {
	for i < len(s) && (isSpaceByte(s[i]) || strings.IndexByte(also, s[i]) >= 0) {
		i++
	}
	return i
}
