package analysis

// Canonical LLM-facing prose for JS defects reported by up to three surfaces:
// the save-time write gate (tools/content), the `check` action, and the
// post-build static validator (engine/validator_static.go). Single-homing the
// shared clauses here keeps the copies from drifting; a surface with a richer,
// case-specific steer leads with that and uses these as the frame it sits in.

import (
	"fmt"
	"strings"
)

// ExplainJSSyntaxError is the canonical one-line finding for a file that does not
// parse. Used by the `check` action and the post-build validator; the save gate
// rejects with its own richer parse-error shape.
func ExplainJSSyntaxError(file, detail string) string {
	return fmt.Sprintf("`%s` has a JavaScript syntax error: %s. Fix it at the reported line.", file, detail)
}

// ExplainInlineScriptSyntaxError is the canonical finding for a page/layout/component
// whose inline <script> does not parse. docLine is the line in the rendered document,
// which is what the model must edit - the script has no file path of its own.
func ExplainInlineScriptSyntaxError(docPath, detail string, docLine int) string {
	return fmt.Sprintf("`%s` has a JavaScript syntax error in an inline <script>: %s (document line %d). Fix it at the reported line.", docPath, detail, docLine)
}

// ExplainJSBindingError is the canonical finding for binding-level faults the bundler
// proves but a single-file parse cannot see - reassigning a `const`, assigning to an
// imported binding, a duplicate lexical declaration.
func ExplainJSBindingError(file string, hard []string) string {
	return fmt.Sprintf("`%s` has a binding error the browser throws on at runtime (a plain syntax parse does not catch it): %s. Fix the named line(s).", file, strings.Join(hard, "; "))
}

// CycleTreeNote states the import-tree shape and why a cycle is a defect even when the
// page still boots. It is the frame every cycle message sits in; the mechanics of a
// split are owned by the client_js_split guide, which this must not restate.
const CycleTreeNote = "Imports must form a one-directional tree with the entry as the unimported root - the entry imports the leaves, a leaf NEVER imports back from the entry, and two leaves never import each other. The names resolve, but a cycle makes module-eval order brittle: the page can boot with its wiring silently dead (a registry the entry sets up in its body runs after the hoisted leaf imports that fill it)."

// CyclePeerRemedy is the fix for a loop between feature leaves, where no member is the
// page entry: two modules that each trigger the other (a chat leaf that must re-render
// the verdict after a reply while the verdict leaf calls back into chat). The call that
// points back becomes a notification through the zero-import store module; relocating a
// leaf's whole API into the store is the wrong fix for this shape.
const CyclePeerRemedy = "Keep ONE direction as the plain import and turn the other into a notification, so the module being told is never imported: either the notifier updates state in the zero-import store module and the listener re-renders from the store's subscribe/onState callback, or the store declares `export const hooks = { onReply: null };` - the listener assigns `hooks.onReply = fn` in its init and the notifier calls `hooks.onReply(...)` directly, unguarded (no `if (hooks.onReply)` or `?.`): a hook nobody assigned must throw, so the probe and flows catch the missing wire. State, helpers or constants both modules use move into the store module itself. A dynamic `import()` or a `window` global is still the same loop and is rejected too."

// CycleLeafRemedy is the generic fix, used when a surface cannot tell which shape the
// cycle has. The save gate replaces it with a shape-specific instruction.
const CycleLeafRemedy = "If a leaf imports back from the page entry, move the symbol(s) it reaches for DOWN into a zero-import store module - one that imports no other project module - `export` them there, and import them from it, never from the entry. If two feature modules import each other: " + CyclePeerRemedy

// ExplainModuleCycle is the canonical whole-graph cycle finding for the batch surfaces
// (the `check` action and the post-build validator), which see a loop but not the edit
// that closed it.
func ExplainModuleCycle(cycle []string) string {
	return fmt.Sprintf("the JS module graph has an import cycle (%s). %s %s",
		strings.Join(cycle, " -> "), CycleTreeNote, CycleLeafRemedy)
}
