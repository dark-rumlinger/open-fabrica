package content

import (
	"strings"
	"testing"
)

// cleanRenderer is a minimal module that parses, modelled on the real artifact:
// two adjacent exported functions.
const cleanRenderer = `export function blendColor(a, b) {
  return a + b;
}

export function renderMissionMarker(ctx, x, y) {
  ctx.fillRect(x, y, 1, 1);
}
`

// decapitatedRenderer is what a greedy patch (empty replace spanning into the
// next declaration) leaves behind: the "export function renderMissionMarker"
// header is gone, leaving an orphan parameter list before the brace.
const decapitatedRenderer = `export function blendColor(a, b) {
  return a + b;
}

(ctx, x, y) {
  ctx.fillRect(x, y, 1, 1);
}
`

func TestRejectIfBrokeSyntax_BlocksDecapitatedFunction(t *testing.T) {
	rej := rejectIfBrokeSyntax("file", "js/game/renderer.js", cleanRenderer, decapitatedRenderer, true)
	if rej == nil {
		t.Fatal("expected the decapitating edit to be rejected, got nil")
	}
	if rej.Success {
		t.Error("rejection Result should have Success=false")
	}
	if rej.Code != "syntax_error" {
		t.Errorf("Code = %q, want %q", rej.Code, "syntax_error")
	}
	if rej.Error == "" {
		t.Error("expected a non-empty error describing the breakage")
	}
}

// TestSyntaxHint covers the two remediation hints the write gate appends to a
// raw parser message: a function declared inside an `export { ... }` list, and
// an apostrophe terminating a single-quoted string. The hint carries the fix on
// the rejection itself rather than as prose in the client_js_split guide, so the
// guidance costs tokens only when the mistake actually happens.
func TestSyntaxHint(t *testing.T) {
	exportTrap := "function init() {}\nexport { init, function getScene() { return scene; } }"
	if hint := syntaxHint(exportTrap, `Expected "}" but found "function" (2:16)`); !strings.Contains(hint, "export it by name") {
		t.Errorf("export-list trap must hint the declare-then-export fix, got %q", hint)
	}

	apostropheTrap := "const a = 1;\nconst msg = 'It'll save';"
	if hint := syntaxHint(apostropheTrap, `Expected ";" but found "ll" (2:16)`); !strings.Contains(hint, "double quotes or template literals") {
		t.Errorf("apostrophe trap must hint the quoting fix, got %q", hint)
	}

	// An unrelated parse error must read exactly as before - no hint.
	if hint := syntaxHint("const x = {;", `Unexpected ";" (1:11)`); hint != "" {
		t.Errorf("unrelated error must get no hint, got %q", hint)
	}
	// The apostrophe pattern on a line OTHER than the reported one must not hint:
	// the hint is scoped to the offending line to avoid mislabeling.
	elsewhere := "const msg = 'It'll save';\nconst y = }{;"
	if hint := syntaxHint(elsewhere, `Unexpected "}" but found "{" (2:11)`); hint != "" {
		t.Errorf("pattern away from the reported line must get no hint, got %q", hint)
	}
}

func TestRejectIfBrokeSyntax_AllowsCleanEdit(t *testing.T) {
	edited := cleanRenderer + "\nexport const VERSION = 2;\n"
	if rej := rejectIfBrokeSyntax("file", "js/game/renderer.js", cleanRenderer, edited, true); rej != nil {
		t.Fatalf("clean ESM edit should pass, got rejection: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_AllowsImportExport(t *testing.T) {
	body := `import { foo } from './foo.js';
export function bar() { return foo(); }
`
	if rej := rejectIfBrokeSyntax("file", "js/app.js", "", body, false); rej != nil {
		t.Fatalf("top-level import/export should parse clean, got rejection: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_DoesNotTrapAlreadyBroken(t *testing.T) {
	// The file was already broken before this edit, so a partial repair that is
	// still broken must not be blocked.
	stillBroken := decapitatedRenderer + "\nfunction "
	if rej := rejectIfBrokeSyntax("file", "js/game/renderer.js", decapitatedRenderer, stillBroken, true); rej != nil {
		t.Fatalf("edit on an already-broken file should not be blocked, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_IgnoresNonCodePaths(t *testing.T) {
	// A path the gate does not cover passes through even with broken content.
	if rej := rejectIfBrokeSyntax("file", "data/notes.txt", "ok", "(x) {", true); rej != nil {
		t.Fatalf("non-code path should not be gated, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_IgnoresNoOp(t *testing.T) {
	if rej := rejectIfBrokeSyntax("file", "js/app.js", cleanRenderer, cleanRenderer, true); rej != nil {
		t.Fatalf("identical body should be treated as a no-op, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_BlocksUnbalancedCSS(t *testing.T) {
	// An edit that leaves a CSS block unclosed drops every rule after it - the one
	// structural CSS error browsers do not recover from - so the gate refuses it.
	const cleanCSS = `.a { color: red; }`
	const unbalanced = `.a { color: red;` // missing closing brace
	rej := rejectIfBrokeSyntax("file", "css/styles.css", cleanCSS, unbalanced, true)
	if rej == nil {
		t.Fatal("a clean->unbalanced CSS edit must be rejected")
	}
	if rej.Code != "syntax_error" {
		t.Fatalf("want code syntax_error, got %q", rej.Code)
	}
}

func TestRejectIfBrokeSyntax_AllowsRecoverableCSS(t *testing.T) {
	// A property/value typo keeps the braces balanced; browsers just ignore it, so
	// the gate must NOT trap an otherwise-legitimate edit.
	const cleanCSS = `.a { color: red; }`
	const typo = `.a { colour: rde; }`
	if rej := rejectIfBrokeSyntax("file", "css/styles.css", cleanCSS, typo, true); rej != nil {
		t.Fatalf("a balanced CSS edit (mere typo) must not be gated, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_DoesNotTrapAlreadyBrokenCSS(t *testing.T) {
	// Unbalanced before the edit: a partial repair that is still unbalanced must
	// not be trapped, mirroring the JS already-broken case.
	const broken = `.a { color: red;`
	const stillBroken = `.a { color: blue;`
	if rej := rejectIfBrokeSyntax("file", "css/styles.css", broken, stillBroken, true); rej != nil {
		t.Fatalf("an in-progress CSS repair must not be trapped, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_CreateRejectionSteersToSave(t *testing.T) {
	// A save that was CREATING a new file (exists=false) is rejected for a syntax
	// error. Nothing was stored, so the file does not exist - the recovery hint
	// must steer back to a full-body save, never a patch (which would only bounce
	// off notFoundForModify). This is the exact create-rejected -> patch ->
	// not-found dance the exists flag removes.
	rej := rejectIfBrokeSyntax("file", "js/app.js", "", decapitatedRenderer, false)
	if rej == nil {
		t.Fatal("expected the broken create to be rejected, got nil")
	}
	if rej.Code != "syntax_error" {
		t.Errorf("Code = %q, want %q", rej.Code, "syntax_error")
	}
	for _, want := range []string{"save rejected", "NOT created", "call save again"} {
		if !strings.Contains(rej.Error, want) {
			t.Errorf("create rejection should mention %q; got: %s", want, rej.Error)
		}
	}
	if strings.Contains(rej.Error, "adjust your edit") {
		t.Errorf("create rejection must not use the edit-path hint (it sends the model to patch); got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_EditRejectionKeepsAdjustHint(t *testing.T) {
	// An edit of an EXISTING artifact (exists=true) keeps the patch-friendly hint:
	// the file is still there, so adjust-and-retry is the correct next step.
	rej := rejectIfBrokeSyntax("file", "js/game/renderer.js", cleanRenderer, decapitatedRenderer, true)
	if rej == nil {
		t.Fatal("expected the decapitating edit to be rejected, got nil")
	}
	if !strings.Contains(rej.Error, "adjust your edit") {
		t.Errorf("edit rejection should keep the adjust-and-retry hint; got: %s", rej.Error)
	}
	if strings.Contains(rej.Error, "was NOT created") {
		t.Errorf("edit rejection must not use the create-path hint; got: %s", rej.Error)
	}
}

// cleanPage / brokenPage model a page artifact (kind=page, path=a route) whose JS
// lives in an inline <script> - the case the path-keyed gate alone never covered.
const cleanPage = `<main>
<script>
  const x = 1;
  document.title = "ok" + x;
</script>
</main>`

const brokenPage = `<main>
<script>
  const x = ;
</script>
</main>`

func TestRejectIfBrokeSyntax_BlocksBrokenInlineScript(t *testing.T) {
	rej := rejectIfBrokeSyntax("page", "/", cleanPage, brokenPage, true)
	if rej == nil {
		t.Fatal("a clean->broken inline <script> edit must be rejected")
	}
	if rej.Code != "syntax_error" {
		t.Errorf("Code = %q, want syntax_error", rej.Code)
	}
	if !strings.Contains(rej.Error, "inline <script>") {
		t.Errorf("error should mention the inline <script>; got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_AllowsInlineScriptRepair(t *testing.T) {
	if rej := rejectIfBrokeSyntax("page", "/", brokenPage, cleanPage, true); rej != nil {
		t.Fatalf("fixing a broken inline script should pass, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_AllowsCleanInlineScriptEdit(t *testing.T) {
	edited := strings.Replace(cleanPage, `"ok"`, `"okk"`, 1)
	if rej := rejectIfBrokeSyntax("page", "/", cleanPage, edited, true); rej != nil {
		t.Fatalf("a clean inline-script edit should pass, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_BlocksBrokenScriptAddedToScriptlessPage(t *testing.T) {
	// The pre-edit page had NO inline script (not gated => treated as clean); adding a
	// broken one must still read as clean->broken and be rejected, not waved through.
	const scriptless = `<main><p>hello</p></main>`
	rej := rejectIfBrokeSyntax("page", "/", scriptless, brokenPage, true)
	if rej == nil {
		t.Fatal("adding a broken inline script to a scriptless page must be rejected")
	}
	if rej.Code != "syntax_error" {
		t.Errorf("Code = %q, want syntax_error", rej.Code)
	}
}

func TestRejectIfBrokeSyntax_DoesNotTrapAlreadyBrokenInlineScript(t *testing.T) {
	// Broken before AND after: an in-progress repair must not be trapped.
	stillBroken := strings.Replace(brokenPage, "const x = ;", "const x = ;\n  const y = ;", 1)
	if rej := rejectIfBrokeSyntax("page", "/", brokenPage, stillBroken, true); rej != nil {
		t.Fatalf("an in-progress inline-script repair must not be trapped, got: %s", rej.Error)
	}
}

func TestRejectIfBrokeSyntax_ScriptlessPageNotGated(t *testing.T) {
	// A page with no executable inline script is not gated - there is no JS to parse,
	// so an HTML-only edit passes through.
	const a = `<main><p>one</p></main>`
	const b = `<main><p>two</p><span>(x) {</span></main>`
	if rej := rejectIfBrokeSyntax("page", "/", a, b, true); rej != nil {
		t.Fatalf("a scriptless page must not be gated, got: %s", rej.Error)
	}
}

// exportListWithFunc reproduces the recurring authoring mistake: a function
// declaration placed inside an ES `export { ... }` list, which is a SyntaxError
// (`Expected "}" but found "getScene"`).
const exportListWithFunc = `let scene;
function init() { scene = 1; }
export {
  init,
  function getScene() { return scene; }
};
`

func TestRejectIfBrokeSyntax_ExportListFunctionHint(t *testing.T) {
	rej := rejectIfBrokeSyntax("file", "files/builder.scene.js", "", exportListWithFunc, false)
	if rej == nil {
		t.Fatal("expected a function-in-export-list body to be rejected, got nil")
	}
	if rej.Code != "syntax_error" {
		t.Errorf("Code = %q, want syntax_error", rej.Code)
	}
	// The rejection must carry the targeted export-list steer.
	for _, want := range []string{"export { ... }", "export it by name"} {
		if !strings.Contains(rej.Error, want) {
			t.Errorf("rejection should carry the export-list hint mentioning %q; got: %s", want, rej.Error)
		}
	}
}

func TestRejectIfBrokeSyntax_NoExportHintForUnrelatedError(t *testing.T) {
	// The targeted hint is additive: an unrelated syntax error (a decapitated
	// function, no export list) must NOT be decorated with the export-list steer.
	rej := rejectIfBrokeSyntax("file", "js/game/renderer.js", cleanRenderer, decapitatedRenderer, true)
	if rej == nil {
		t.Fatal("expected the decapitating edit to be rejected, got nil")
	}
	if strings.Contains(rej.Error, "export it by name") {
		t.Errorf("an unrelated syntax error must not carry the export-list hint; got: %s", rej.Error)
	}
}
