package analysis

import (
	"strings"
	"testing"
)

func findFunc(spans []FuncSpan, name string) (FuncSpan, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return FuncSpan{}, false
}

func TestJSFunctions_EmptyExportedMethods(t *testing.T) {
	// Mirrors the real stub shape: a class with comment-only / empty methods.
	src := `export default class Landing {
  constructor(app) {
    this.app = app;
  }
  init() {
    // TODO: implement in body phase
  }
  _validateRoomName(name) {
    return { valid: false };
  }
  render() {}
}`
	spans := JSFunctions(src)

	if s, ok := findFunc(spans, "init"); !ok || !s.BodyEmpty || s.Kind != "method" {
		t.Fatalf("init must be an empty method, got %+v (ok=%v)", s, ok)
	}
	if s, ok := findFunc(spans, "render"); !ok || !s.BodyEmpty {
		t.Fatalf("render() {} must be empty, got %+v (ok=%v)", s, ok)
	}
	if s, ok := findFunc(spans, "_validateRoomName"); !ok || s.BodyEmpty {
		t.Fatalf("_validateRoomName has a return statement, must NOT be empty, got %+v", s)
	}
	// constructor is excluded (an empty one is a harmless no-op, not a stub).
	if _, ok := findFunc(spans, "constructor"); ok {
		t.Fatalf("constructor should not be reported")
	}
}

func TestJSFunctions_TopLevelFunctionsAndArrows(t *testing.T) {
	src := `export function start() {
  console.log('go');
}
function helper() {
  /* nothing yet */
}
export const boot = () => {
  init();
};
const noop = () => {};`
	spans := JSFunctions(src)

	if s, ok := findFunc(spans, "start"); !ok || !s.Exported || s.BodyEmpty {
		t.Fatalf("start: want exported non-empty, got %+v (ok=%v)", s, ok)
	}
	if s, ok := findFunc(spans, "helper"); !ok || s.Exported || !s.BodyEmpty {
		t.Fatalf("helper: want unexported empty (comment-only), got %+v (ok=%v)", s, ok)
	}
	if s, ok := findFunc(spans, "boot"); !ok || !s.Exported || s.BodyEmpty {
		t.Fatalf("boot: want exported non-empty arrow, got %+v (ok=%v)", s, ok)
	}
	if s, ok := findFunc(spans, "noop"); !ok || !s.BodyEmpty {
		t.Fatalf("noop: want empty arrow, got %+v (ok=%v)", s, ok)
	}
}

func TestJSFunctions_IgnoresControlBlocksAndStrings(t *testing.T) {
	src := `export function run(x) {
  if (x) {
    doThing();
  }
  for (let i = 0; i < 3; i++) {
    step(i);
  }
  const s = "a function() {} inside a string";
  const t = ` + "`template ${x} with { braces }`" + `;
}`
	spans := JSFunctions(src)

	if len(spans) != 1 || spans[0].Name != "run" {
		t.Fatalf("expected only run() detected, got %+v", spans)
	}
	if spans[0].BodyEmpty {
		t.Fatalf("run has real statements, must not be empty")
	}
}

func TestJSFunctions_LineNumbers(t *testing.T) {
	src := "line1\nfunction f() {\n  go();\n}\n"
	spans := JSFunctions(src)
	if len(spans) != 1 {
		t.Fatalf("expected 1 function, got %+v", spans)
	}
	if spans[0].StartLine != 2 || spans[0].EndLine != 4 {
		t.Fatalf("line span wrong: got start=%d end=%d want 2..4", spans[0].StartLine, spans[0].EndLine)
	}
}

func TestMaskJS_BlanksCommentsAndStrings(t *testing.T) {
	src := "a // x{y}\n/* b{ */ c '{}' `t{}`"
	mask := maskJS(src)
	if len(mask) != len(src) {
		t.Fatalf("mask must preserve length: %d vs %d", len(mask), len(src))
	}
	// Braces inside comments and strings must be blanked so brace matching is safe.
	for i, c := range mask {
		if c == '{' || c == '}' {
			t.Fatalf("brace survived masking at %d: %q", i, mask)
		}
	}
	// Newlines preserved (the \n after the line comment is at offset 9).
	if mask[9] != '\n' {
		t.Fatalf("newline not preserved, got %q", mask[9])
	}
}

// A regex literal whose body contains a quote (/'/g) must be recognized as a
// regex, not a string: treating its `'` as a delimiter would invert string/code
// parity and leak a later string's interior out as code.
func TestMaskJS_RegexLiteralDoesNotDesyncStrings(t *testing.T) {
	// Faithful to the failing file: a double-quoted string holding a `'`, then the
	// `/'/g` regex, then a single-quoted string carrying the `empty-state` class.
	src := "function f(pid){\n" +
		"  var sql = \"id = '\" + pid.replace(/'/g, '') + \"'\";\n" +
		"  el.innerHTML = '<div class=\"empty-state\">x</div>';\n" +
		"}\n"
	mask := maskJS(src)
	if len(mask) != len(src) {
		t.Fatalf("mask must preserve length: %d vs %d", len(mask), len(src))
	}
	// The class name lives inside a string literal; its interior must be blanked,
	// so the bare `state` token never survives as code.
	if strings.Contains(mask, "state") {
		t.Fatalf("string interior leaked through masking (regex desync): %q", mask)
	}
}

// A template literal whose interpolation holds a nested template literal. A
// scanner that blanked to the next backtick would mistake the nested opener for
// the terminator and expose HTML attribute text as code. The attribute text must
// stay blanked; the code inside `${...}` must stay exposed.
func TestMaskJS_NestedTemplateDoesNotDesyncStrings(t *testing.T) {
	src := "export function render(items) {\n" +
		"  return `<ul>${items.map(it => `<li data-row-id=\"${it.id}\">x</li>`).join('')}</ul>`;\n" +
		"}\n"
	mask := maskJS(src)
	if len(mask) != len(src) {
		t.Fatalf("mask must preserve length: %d vs %d", len(mask), len(src))
	}
	// HTML attribute text lives inside template string content and must be blanked,
	// so `data-row-id="..."` never survives as a bare `row`/`id` assignment target.
	if strings.Contains(mask, "data-row-id") {
		t.Fatalf("template string text leaked through masking (backtick desync): %q", mask)
	}
	// Interpolation code stays exposed - `join` in particular proves parity resumed
	// correctly in code mode after the nested template closed.
	for _, want := range []string{"items", "map", "join"} {
		if !strings.Contains(mask, want) {
			t.Fatalf("interpolation identifier %q was blanked, want exposed: %q", want, mask)
		}
	}
}

// Division must NOT be mistaken for a regex literal: a `/` after a value (an
// identifier, number, `)`, `]`, or a string) is the divide operator, and treating
// the rest of the line as a regex body would blank real code.
func TestMaskJS_DivisionIsNotRegex(t *testing.T) {
	src := "var r = total / count;\nvar s = 'keep me';\n"
	mask := maskJS(src)
	if len(mask) != len(src) {
		t.Fatalf("mask must preserve length: %d vs %d", len(mask), len(src))
	}
	// The division operands and the later string delimiters must be intact; if the
	// `/` were read as a regex start it would swallow `count;` and the next quote.
	if !strings.Contains(mask, "total ") || !strings.Contains(mask, "count;") {
		t.Fatalf("division operands were blanked as a regex body: %q", mask)
	}
}
