package analysis

// Canonical LLM-facing prose for unresolved-import findings: every batch
// surface (the post-build static validator, the check action) renders an
// analysis.ImportIssue through RenderImportProblems, so a dangling import reads
// the same wherever it is caught. The save-time write gate is the deliberate
// exception: its single-issue reject is richer by design and stays where it is.

import (
	"fmt"
	"strings"
)

// explainImportIssue returns the canonical one-line explanation for a single
// unresolved-import finding, dispatched on is.Reason: a name_missing issue names
// the target's export surface; any other reason (target_absent) says nothing
// resolves the specifier. The check surface only ever passes name_missing issues.
func explainImportIssue(is ImportIssue) string {
	if is.Reason == "name_missing" {
		surface := "it exports nothing"
		if len(is.Exports) > 0 {
			surface = "it exports: " + strings.Join(is.Exports, ", ")
		}
		if is.HasDefault {
			surface += " (plus a default export)"
		}
		return fmt.Sprintf("imports `%s` from `%s`, but that module declares no such export (%s)", is.Imported, is.Spec, surface)
	}
	return fmt.Sprintf("imports `%s` from `%s`, but no module resolves that specifier (save the target at the path the specifier points to, or fix the path)", is.Imported, is.Spec)
}

// RenderImportProblems builds the multi-issue message a batch surface (the
// post-build static validator and the check action) shows for one file's
// unresolved imports, listing each so a single fix-pass corrects them all. One
// footer covers both failure shapes (a wrong name and a missing target), so every
// surface closes with the same instruction.
func RenderImportProblems(file string, probs []ImportIssue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module `%s` has unresolved imports - the browser throws when it links the graph:", file)
	for _, is := range probs {
		fmt.Fprintf(&b, "\n  - %s", explainImportIssue(is))
	}
	b.WriteString("\nFix each import name, or add the missing export to (or create/relocate) the target module it points to.")
	return b.String()
}
