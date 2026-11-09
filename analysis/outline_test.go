package analysis

import (
	"strings"
	"testing"
)

// A brace inside a string or comment must not shift function-body depth. This is
// the core bug the lexer-backed masking fixes: the naive counter saw the `}` in
// `"}"` and closed the function a line in, corrupting every signature after it.
func TestOutlineJS_BraceInStringDoesNotCloseBlock(t *testing.T) {
	src := strings.Join([]string{
		"export function render(items) {",
		`  const a = "}";`,
		"  const b = '{';",
		"  return a + b;",
		"}",
	}, "\n")

	out := outlineJS(src)
	if !strings.Contains(out, "export function render(items) {  <- 4 lines") {
		t.Errorf("function with brace-bearing strings should collapse as one 4-line block, got:\n%s", out)
	}
	if !strings.Contains(out, "Functions: 1") {
		t.Errorf("expected exactly one function counted, got:\n%s", out)
	}
}

// A multi-line template literal carries braces that are not code (including a
// `}` on its own line); masking must keep the surrounding function intact.
func TestOutlineJS_TemplateBracesIgnored(t *testing.T) {
	src := strings.Join([]string{
		"function f(x) {",
		"  const t = `a${x}b{c}`;",
		"  const u = `line1",
		"  still in template }`;",
		"  return t;",
		"}",
	}, "\n")

	out := outlineJS(src)
	if !strings.Contains(out, "Functions: 1") {
		t.Errorf("template braces must not split the function, got:\n%s", out)
	}
	// The closing brace is on line 6, so the block spans 5 body lines.
	if !strings.Contains(out, "<- 5 lines") {
		t.Errorf("function should span its real 5 body lines, got:\n%s", out)
	}
}

// A module wrapped in an IIFE must still surface its inner functions. Before the
// nested-aware outliner, the whole body collapsed to a single "(function () {"
// line reporting "Functions: 0", leaving the model with no structural map and
// forcing it to paginate the file blindly.
func TestOutlineJS_IIFEInnerFunctionsSurfaced(t *testing.T) {
	src := strings.Join([]string{
		"// engine",
		"(function () {",
		"  'use strict';",
		"  var COLS = 10;",
		"  function reset() {",
		"    grid = [];",
		"    score = 0;",
		"    lines = 0;",
		"    level = 1;",
		"  }",
		"  function drawAll() {",
		"    ctx.clearRect(0, 0, w, h);",
		"    drawGrid();",
		"    drawPiece();",
		"  }",
		"  var api = {",
		"    start: function () {",
		"      reset();",
		"      drawAll();",
		"      running = true;",
		"    },",
		"  };",
		"  window.Game = api;",
		"})();",
	}, "\n")

	out := outlineJS(src)
	for _, want := range []string{"function reset()", "function drawAll()", "start: function ()"} {
		if !strings.Contains(out, want) {
			t.Errorf("IIFE outline should surface %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Functions: 0") {
		t.Errorf("IIFE-wrapped module must not report zero functions, got:\n%s", out)
	}
}

// Control-flow blocks inside a function must not be mistaken for nested functions
// (no false positives from `if`/`for` method-shorthand lookalikes).
func TestOutlineJS_ControlFlowNotCountedAsFunctions(t *testing.T) {
	src := strings.Join([]string{
		"function tick(dt) {",
		"  if (paused) {",
		"    return;",
		"  }",
		"  for (var i = 0; i < n; i++) {",
		"    step(i);",
		"  }",
		"}",
	}, "\n")

	out := outlineJS(src)
	if !strings.Contains(out, "Functions: 1") {
		t.Errorf("expected exactly one function (tick), if/for must not count, got:\n%s", out)
	}
}

// A top-level multi-line declaration (a `const x = {...}` block) must survive in
// the skeleton: it takes the block path and is not a function, so the outline
// must still emit its symbol name rather than dropping it entirely.
func TestOutlineJS_TopLevelMultilineDeclSurvives(t *testing.T) {
	src := strings.Join([]string{
		"const oceanMat = new THREE.MeshPhongMaterial({",
		"  color: 0x1e6091,",
		"  shininess: 80,",
		"  flatShading: true,",
		"  transparent: true,",
		"});",
		"const ocean = new THREE.Mesh(oceanGeo, oceanMat);",
	}, "\n")

	out := outlineJS(src)
	if !strings.Contains(out, "1: const oceanMat = new THREE.MeshPhongMaterial({  <- 5 lines") {
		t.Errorf("top-level multi-line decl should collapse to a named summary, got:\n%s", out)
	}
	if !strings.Contains(out, "const ocean = new THREE.Mesh") {
		t.Errorf("the following single-line decl should still print, got:\n%s", out)
	}
}

// A flat, function-light module must not reproduce itself: runs of 3+ plain
// wiring statements collapse to one summary line, while declarations, comments
// and functions stay as the structural anchors.
func TestOutlineJS_FlatStatementRunsCollapse(t *testing.T) {
	src := strings.Join([]string{
		"// setup",
		"const boat = new THREE.Group();",
		"boat.add(hull);",
		"boat.add(mast);",
		"boat.add(sail);",
		"boat.position.set(0, 0.5, 0);",
		"scene.add(boat);",
	}, "\n")

	out := outlineJS(src)
	if !strings.Contains(out, "// setup") {
		t.Errorf("comment anchor should be kept, got:\n%s", out)
	}
	if !strings.Contains(out, "2: const boat = new THREE.Group();") {
		t.Errorf("declaration should be kept verbatim, got:\n%s", out)
	}
	// Lines 3-7 are five bare statements: they must fold into one summary.
	if !strings.Contains(out, "3-7: ... (5 statements)") {
		t.Errorf("run of plain statements should collapse, got:\n%s", out)
	}
	if strings.Contains(out, "boat.add(hull)") {
		t.Errorf("collapsed statements must not appear individually, got:\n%s", out)
	}
}

// A short run (<=2) of plain statements is not worth a summary line, so it
// prints verbatim - collapsing two lines into one saves nothing.
func TestOutlineJS_ShortStatementRunPrintsVerbatim(t *testing.T) {
	src := strings.Join([]string{
		"const r = makeRenderer();",
		"r.setPixelRatio(2);",
		"r.setSize(800, 600);",
	}, "\n")

	out := outlineJS(src)
	for _, want := range []string{"r.setPixelRatio(2);", "r.setSize(800, 600);"} {
		if !strings.Contains(out, want) {
			t.Errorf("short statement run should print verbatim, missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "statements)") {
		t.Errorf("a 2-line run should not be collapsed, got:\n%s", out)
	}
}

// A brace inside a CSS value (content: "}") must not close the rule block early.
func TestOutlineCSS_BraceInValueDoesNotCloseBlock(t *testing.T) {
	src := strings.Join([]string{
		".btn {",
		`  content: "}";`,
		"  color: red;",
		"  margin: 0;",
		"  padding: 0;",
		"}",
		".x { color: blue; }",
	}, "\n")

	out := outlineCSS(src)
	if !strings.Contains(out, ".btn {  <- 5 lines") {
		t.Errorf(".btn block (with a brace in a value) should collapse as one 5-line block, got:\n%s", out)
	}
	if !strings.Contains(out, ".x { color: blue; }") {
		t.Errorf("the following single-line rule should still be parsed, got:\n%s", out)
	}
}

// HTML is parsed with a real tokenizer: a structural tag that is not at line
// start is found, markup inside an inline <script> string is NOT counted as
// structure, and interactive elements are tallied accurately.
func TestOutlineHTML_RealTokenizer(t *testing.T) {
	src := strings.Join([]string{
		`<header><nav id="top"></nav></header>`,
		`<div class="grid">`,
		"  <button>Go</button>",
		"</div>",
		`<script>const x = "<div>not real</div>";</script>`,
	}, "\n")

	out := outlineHTML(src)
	for _, want := range []string{`<nav id="top">`, `<div class="grid">`, "1 button(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("HTML outline missing %q, got:\n%s", want, out)
		}
	}
	// The <div> inside the inline script is a string, not real structure: exactly
	// one opening div (the grid) should appear.
	if got := strings.Count(out, "<div"); got != 1 {
		t.Errorf("expected exactly 1 structural <div (script-string markup must not count), got %d:\n%s", got, out)
	}
}

// FileTypeForOutline routes by content-type first, then extension, defaulting to
// the (forgiving) HTML outliner.
func TestFileTypeForOutline(t *testing.T) {
	cases := []struct{ path, ct, want string }{
		{"a.js", "", "js"},
		{"a.mjs", "", "js"},
		{"a.css", "", "css"},
		{"styles", "text/css; charset=utf-8", "css"},
		{"script", "application/javascript", "js"},
		{"/", "text/html", "html"},
		{"unknown", "", "html"},
	}
	for _, c := range cases {
		if got := FileTypeForOutline(c.path, c.ct); got != c.want {
			t.Errorf("FileTypeForOutline(%q,%q)=%q want %q", c.path, c.ct, got, c.want)
		}
	}
}
