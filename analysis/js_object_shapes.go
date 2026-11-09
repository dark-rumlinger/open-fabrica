package analysis

import (
	"regexp"
	"strings"
)

// The DATA-contract analog of ExtractNamedExports: reports the field KEYS of
// the objects a module's exports carry - the shape a sibling reads/writes
// across the module boundary, which no symbol gate (name, import, arity)
// captures. Surfaced in project_state so a sibling authored a turn later wires
// to the real field set.
//
// Names only, never values. Two shapes are detected: an exported object-literal
// binding and an exported function whose body returns an object literal. Only
// bare identifier keys are reported (a computed, spread, string, or numeric key
// names no static field a sibling can rely on); keys the module adds by later
// assignment count too, nested to maxObjectShapeDepth.

// maxObjectShapeKeys caps the keys reported for one object so a large config/lookup map
// (e.g. a 200-entry table) cannot flood the snapshot; the overflow is marked with an
// ellipsis, like the DOM-id and style caps.
const maxObjectShapeKeys = 40

// maxObjectShapeDepth is how many levels of nested object literal are reported.
// A shared store module's state is almost always nested and the producer/consumer
// mismatch this exists to catch lives INSIDE the nested bag, so depth 1 defeats
// the purpose; deeper than 2 is where a config tree would start flooding the
// snapshot for no contract value.
const maxObjectShapeDepth = 2

// maxNestedShapeKeys caps a NESTED object's keys, tighter than the top level: the parent
// cap multiplies (40 x 40 would be 1600 keys on one line), and a nested bag that large is
// a data table, not a contract.
const maxNestedShapeKeys = 12

// ExtractExportedObjectShapes returns, keyed by export name, the field keys of each
// exported object in body - both a `export const|let|var X = { ... }` binding and an
// exported function whose top-level `return { ... }` yields a literal - nested to
// maxObjectShapeDepth and including keys the module assigns on later. An export that
// carries no object literal, or whose shape resolves to nothing, gets no entry; the
// result is nil when there are none.
func ExtractExportedObjectShapes(body string) map[string][]string {
	masked := maskJS(body)
	out := map[string][]string{}

	// (1) exported object-literal bindings: export const|let|var X = { ... }
	for _, loc := range exportVarRe.FindAllStringIndex(masked, -1) {
		name, keys, ok := firstObjectDeclarator(masked, loc[1])
		if ok {
			out[name] = keys
		}
	}

	// (2) exported factory functions: export function f(...) { ...; return { ... } }
	lineStarts := computeLineStarts(masked)
	for _, fs := range JSFunctions(body) {
		if !fs.Exported || fs.Kind != "function" {
			continue
		}
		if _, done := out[fs.Name]; done {
			continue
		}
		lo := lineStarts[fs.StartLine]
		hi := len(masked)
		if fs.EndLine+1 < len(lineStarts) {
			hi = lineStarts[fs.EndLine+1]
		}
		if keys, ok := firstReturnedObjectKeys(masked[lo:hi]); ok {
			out[fs.Name] = keys
		}
	}

	// An exported object is often DECLARED empty and filled by assignment - the common
	// `export const dom = {}` + a `cacheDom()` that sets `dom.jumpBtn = ...`. The literal
	// reports nothing, so without this the snapshot renders `dom{  }`, which ASSERTS the
	// object has no fields. That is worse than silence: a sibling reads the shape line as
	// the contract, finds nothing, and re-reads the module to discover what is really
	// cached - the read the shape line exists to prevent.
	for name, keys := range out {
		for _, k := range assignedObjectKeys(masked, name) {
			if !hasBareKey(keys, k) {
				keys = append(keys, k)
				if len(keys) >= maxObjectShapeKeys {
					keys = append(keys, "…")
					break
				}
			}
		}
		out[name] = keys
	}
	// An export whose shape is still empty states nothing; no entry beats `x{  }`.
	for name, keys := range out {
		if len(keys) == 0 {
			delete(out, name)
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// assignedObjectKeys returns the field names the module ASSIGNS onto the exported object
// bound to name (`name.key = ...`), in source order, deduped. Compound and comparison
// operators are excluded: only a plain `=` creates a field. A nested target
// (`name.a.b = 1`) reports `a`, which is the field the sibling actually reads.
func assignedObjectKeys(masked, name string) []string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\.([A-Za-z_$][A-Za-z0-9_$]*)[^\S\n]*=(?:[^=]|$)`)
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(masked, -1) {
		if k := m[1]; !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// hasBareKey reports whether keys already carries k, ignoring any nested `{ ... }` a
// literal key may have picked up.
func hasBareKey(keys []string, k string) bool {
	for _, existing := range keys {
		if i := strings.IndexByte(existing, '{'); i >= 0 {
			existing = strings.TrimSpace(existing[:i])
		}
		if existing == k {
			return true
		}
	}
	return false
}

// firstObjectDeclarator reads the first declarator of a `const|let|var` list whose text
// begins at start (just past the `export const ` keyword, on the masked body). It returns
// the bound name and its object-literal keys when that declarator is `name = { ... }`;
// ok is false for a destructuring target, a non-object initializer, or a bare declaration.
// Only the first declarator is inspected - `export const state = { ... }` is the shape
// that matters; a multi-declarator list with several object literals is not chased.
func firstObjectDeclarator(masked string, start int) (name string, keys []string, ok bool) {
	i, n := start, len(masked)
	for i < n && isSpaceByte(masked[i]) {
		i++
	}
	if i >= n || !isIdentStartByte(masked[i]) {
		return "", nil, false // destructuring pattern or end of input
	}
	j := i + 1
	for j < n && isIdentByte(masked[j]) {
		j++
	}
	name = masked[i:j]
	i = j
	for i < n && isSpaceByte(masked[i]) {
		i++
	}
	if i >= n || masked[i] != '=' {
		return "", nil, false // no initializer
	}
	i++
	for i < n && isSpaceByte(masked[i]) {
		i++
	}
	if i >= n || masked[i] != '{' {
		return "", nil, false // initializer is not an object literal
	}
	end := skipBalancedJS(masked, i)
	return name, objectLiteralKeys(masked[i:end]), true
}

// firstReturnedObjectKeys returns the keys of the first `return { ... }` object literal in
// fn (a masked function-body slice), or ok=false when the body returns no object literal.
func firstReturnedObjectKeys(fn string) (keys []string, ok bool) {
	for idx := strings.Index(fn, "return"); idx >= 0; {
		after := idx + len("return")
		// Require a word boundary so `returnValue` never matches.
		boundaryBefore := idx == 0 || !isIdentByte(fn[idx-1])
		boundaryAfter := after >= len(fn) || !isIdentByte(fn[after])
		if boundaryBefore && boundaryAfter {
			k := after
			for k < len(fn) && isSpaceByte(fn[k]) {
				k++
			}
			if k < len(fn) && fn[k] == '{' {
				end := skipBalancedJS(fn, k)
				return objectLiteralKeys(fn[k:end]), true
			}
		}
		next := strings.Index(fn[after:], "return")
		if next < 0 {
			return nil, false
		}
		idx = after + next
	}
	return nil, false
}

// objectLiteralKeys returns the identifier field keys of a masked object literal (braces
// included) - covering property (`k: v`), shorthand (`k`), and method (`k() {}`,
// `get k() {}`) forms. A spread, computed, string, or numeric key is skipped. A key whose
// value is itself an object literal recurses to maxObjectShapeDepth and renders inline as
// `k{ a, b }`, so the caller needs no change: the renderer already prints `name{ ... }`
// and a nested key simply carries its own braces. Values are never mined; nesting and
// strings are handled by splitTopLevelCommas. The list is deduped, in source order, capped.
func objectLiteralKeys(masked string) []string {
	return objectLiteralKeysDepth(masked, 1)
}

// nestedShape renders `{ a, b }` for a property whose value is an object literal, or ""
// for anything else (a method, a shorthand, a non-object value, or a depth past the cap).
// rest is the masked text after the key, starting at ':' for a property.
func nestedShape(rest string, depth int) string {
	if depth >= maxObjectShapeDepth || !strings.HasPrefix(rest, ":") {
		return ""
	}
	val := strings.TrimSpace(rest[1:])
	if !strings.HasPrefix(val, "{") {
		return ""
	}
	end := skipBalancedJS(val, 0)
	if end <= 0 || end > len(val) {
		return ""
	}
	sub := objectLiteralKeysDepth(val[:end], depth+1)
	if len(sub) == 0 {
		return "" // an empty nested literal states nothing; `k{  }` would only mislead
	}
	return "{ " + strings.Join(sub, ", ") + " }"
}

func objectLiteralKeysDepth(masked string, depth int) []string {
	if len(masked) < 2 {
		return nil
	}
	limit := maxObjectShapeKeys
	if depth > 1 {
		limit = maxNestedShapeKeys
	}
	inner := masked[1 : len(masked)-1]
	var keys []string
	seen := map[string]bool{}
	for _, el := range splitTopLevelCommas(inner) {
		el = strings.TrimSpace(el)
		if el == "" || strings.HasPrefix(el, "...") {
			continue // trailing comma or spread: no static field name
		}
		// Drop a leading accessor/async/generator prefix on a method shorthand.
		for _, pre := range []string{"get ", "set ", "async ", "static "} {
			el = strings.TrimPrefix(el, pre)
		}
		el = strings.TrimSpace(strings.TrimPrefix(el, "*"))
		if el == "" {
			continue
		}
		if !isIdentStartByte(el[0]) {
			continue // computed `[expr]`, string, or numeric key: not a static identifier field
		}
		j := 1
		for j < len(el) && isIdentByte(el[j]) {
			j++
		}
		key := el[:j]
		rest := strings.TrimSpace(el[j:])
		// A real key is followed by ':' (property), '(' (method), or nothing (shorthand);
		// anything else means the leading token was part of an expression value, not a key.
		if rest != "" && rest[0] != ':' && rest[0] != '(' {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key+nestedShape(rest, depth))
		if len(keys) == limit {
			keys = append(keys, "…")
			break
		}
	}
	return keys
}
