package analysis

// Canonical LLM-facing prose for arity over-application, shared by every surface
// that can catch it, so the model reads one wording wherever it was found. The
// message names no container kind (module vs reducer): only the path differs.

import (
	"fmt"
	"strings"
)

// ExplainArityMismatches builds the actionable finding for same-module functions
// called with more arguments than they accept. Each entry names the call, the
// argument count it was given, and the parameter count the function declares, so
// one fix-pass can correct every site.
func ExplainArityMismatches(file string, mism []ArityMismatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`%s` calls a function it defines with more arguments than the function accepts - JS drops the extra argument, or shifts the others into the wrong parameters (a value then lands where a callback was expected and throws `<x> is not a function` when called):", file)
	for _, m := range mism {
		noun := "parameters"
		if m.Params == 1 {
			noun = "parameter"
		}
		fmt.Fprintf(&b, "\n  - `%s(...)` (line %d) is called with %d arguments, but `%s` declares %d %s", m.Name, m.Line, m.Args, m.Name, m.Params, noun)
	}
	b.WriteString("\nMatch each call to the function's parameter list - pass each argument in the right position, or fix the function's signature.")
	return b.String()
}
