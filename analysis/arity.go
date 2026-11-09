package analysis

// Arity over-application analysis: a same-module function called with MORE
// positional arguments than it can accept. JS silently drops the extras, so the
// bundler and single-file parse stay quiet, yet arguments can shift into the
// wrong slots and only throw at runtime.
//
// Like UndefinedCallRefs, biased HARD toward zero false positives. Reports a
// call only when EVERY guard holds: the callee resolves to exactly ONE
// fixed-arity function DEFINED IN THIS module (never a method or an
// imported/ambiguous/shadowable name); "fixed-arity" means no rest or defaulted
// parameter and no `arguments` read; and the call passes strictly MORE
// top-level args than parameters, none of them a spread. "Too few" is never
// flagged: a trailing optional parameter is idiomatic and legal.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ArityMismatch is one call site that applies a same-module fixed-arity function
// to more positional arguments than it declares parameters.
type ArityMismatch struct {
	Name   string // the called function's name
	Line   int    // 1-based line of the offending call site
	Args   int    // top-level argument count at the call site
	Params int    // declared parameter count of the definition
}

// ArityOverApplications returns every over-applied same-module function call in
// body, sorted by line then name. Pure and deterministic. path is accepted for
// caller symmetry with the other store analyzers; the analysis is body-only.
func ArityOverApplications(path, body string) []ArityMismatch {
	masked := maskJS(body)

	// A name is a candidate only when it is declared EXACTLY ONCE across the whole
	// module - as a function/class/var/let/const or an import. A second binding
	// (a nested `let` of the same name, a shadowing arrow, a colliding import)
	// means a bare call may not reach the definition we measured, so we do not judge.
	declCount := map[string]int{}
	incl := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			declCount[n]++
		}
	}
	for _, m := range funcDeclRe.FindAllStringSubmatch(masked, -1) {
		incl(m[1])
	}
	for _, m := range classDeclRe.FindAllStringSubmatch(masked, -1) {
		incl(m[1])
	}
	for _, loc := range varDeclKwRe.FindAllStringIndex(masked, -1) {
		appendDeclaratorNames(incl, masked[loc[1]:])
	}
	for _, ni := range ExtractNamedImports(masked) {
		incl(ni.Local)
	}
	for _, m := range defaultImportRe.FindAllStringSubmatch(masked, -1) {
		incl(m[1])
	}
	for _, m := range namespaceImportRe.FindAllStringSubmatch(masked, -1) {
		incl(m[1])
	}

	// Index each uniquely-declared, fixed-arity, module-local function by name.
	asg := countAssignments(masked)
	params := map[string]int{}
	for _, fs := range JSFunctions(body) {
		if fs.Kind != "function" || fs.Name == "" || declCount[fs.Name] != 1 || !fixedArity(fs) {
			continue
		}
		if asg.reassigned(fs.Name) {
			continue // a placeholder swapped for the real function later: its first arity is not the callers'
		}
		params[fs.Name] = len(fs.Params)
	}
	if len(params) == 0 {
		return nil
	}

	var out []ArityMismatch
	seen := map[string]bool{} // dedup by name+line
	n := len(masked)
	for i := 0; i < n; {
		if !isIdentStartByte(masked[i]) {
			i++
			continue
		}
		j := i + 1
		for j < n && isIdentByte(masked[j]) {
			j++
		}
		name := masked[i:j]
		want, ok := params[name]
		if !ok {
			i = j
			continue
		}
		// The next significant byte must be `(` for this to be a call.
		k := j
		for k < n && isSpaceByte(masked[k]) {
			k++
		}
		if k >= n || masked[k] != '(' {
			i = j
			continue
		}
		// Property access (`a.foo(...)`) is a method call, not this free function.
		if lastNonSpaceBefore(masked, i) == '.' {
			i = j
			continue
		}
		closeIdx := skipBalancedJS(masked, k) // index just past the matching ')'
		if closeIdx <= k {
			i = j
			continue
		}
		// A definition site (`name(params) {`) is a binding, not a call: skip it so
		// the function's own header is never counted against itself.
		if m := closeIdx; m < n {
			for m < n && isSpaceByte(masked[m]) {
				m++
			}
			if m < n && masked[m] == '{' {
				i = j
				continue
			}
		}
		got := countTopLevelArgs(masked[k+1 : closeIdx-1])
		if got > want {
			line := 1 + strings.Count(masked[:i], "\n")
			key := name + "\x00" + strconv.Itoa(line)
			if !seen[key] {
				seen[key] = true
				out = append(out, ArityMismatch{Name: name, Line: line, Args: got, Params: want})
			}
		}
		i = closeIdx
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Line != out[b].Line {
			return out[a].Line < out[b].Line
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// fixedArity reports whether fs accepts a fixed number of arguments: no rest
// parameter, no defaulted parameter, and no use of the `arguments` object. Only
// such a function has a maximum arg count that an over-application can exceed.
func fixedArity(fs FuncSpan) bool {
	if fs.UsesArguments {
		return false
	}
	for _, p := range fs.Params {
		if strings.HasPrefix(strings.TrimSpace(p), "...") {
			return false // rest parameter: variadic
		}
		if beforeTopLevelAssign(p) != p {
			return false // defaulted parameter: trailing arguments optional
		}
	}
	return true
}

// countTopLevelArgs returns the number of arguments in a call's argument text, or
// -1 when a spread element (`...x`) makes the runtime count unknowable. inner is
// the masked text strictly between the call parens.
func countTopLevelArgs(inner string) int {
	if strings.TrimSpace(inner) == "" {
		return 0
	}
	n := 0
	for _, a := range splitTopLevelCommas(inner) {
		t := strings.TrimSpace(a)
		if t == "" {
			continue // trailing comma
		}
		if strings.HasPrefix(t, "...") {
			return -1 // spread argument: count unknown, do not judge
		}
		n++
	}
	return n
}

// assignments counts, per identifier, the plain assignments (`name = ...`) in a module and
// how many of them are the initializer of a var/let/const declaration.
type assignments struct {
	total, declared map[string]int
}

var (
	assignRe   = regexp.MustCompile(`(^|[^.\w$])([A-Za-z_$][\w$]*)\s*=[^=>]`)
	declInitRe = regexp.MustCompile(`\b(?:var|let|const)\s+([A-Za-z_$][\w$]*)\s*=[^=>]`)
)

// countAssignments scans masked once.
func countAssignments(masked string) assignments {
	a := assignments{total: map[string]int{}, declared: map[string]int{}}
	for _, m := range assignRe.FindAllStringSubmatch(masked, -1) {
		a.total[m[2]]++
	}
	for _, m := range declInitRe.FindAllStringSubmatch(masked, -1) {
		a.declared[m[1]]++
	}
	return a
}

// reassigned reports whether name is assigned to anywhere beyond its own declaration
// (`let hook = () => {}` followed later by `hook = (a, b) => ...`). Such a binding holds
// different functions at different times, so the arity of the first says nothing about a
// call site that runs after the swap.
func (a assignments) reassigned(name string) bool {
	return a.total[name]-a.declared[name] > 0
}
