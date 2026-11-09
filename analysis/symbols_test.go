package analysis

import "testing"

func hasSymbol(syms []Symbol, name, kind string) *Symbol {
	for i := range syms {
		if syms[i].Name == name && syms[i].Kind == kind {
			return &syms[i]
		}
	}
	return nil
}

func TestExtractSymbols_JS(t *testing.T) {
	src := `import { helper, util as u } from './lib.js';

export function renderHeader(opts) {
  return opts;
}

export const TITLE = 'Hi';

class Widget {
  draw() {
    return 1;
  }
}
`
	syms, refs := ExtractSymbols(src, "js")

	if s := hasSymbol(syms, "renderHeader", "js_func"); s == nil {
		t.Fatal("renderHeader js_func not extracted")
	} else if !s.Exported {
		t.Error("renderHeader should be exported")
	} else if s.Line != 3 {
		t.Errorf("renderHeader line = %d, want 3", s.Line)
	}
	if s := hasSymbol(syms, "draw", "js_method"); s == nil {
		t.Error("draw js_method not extracted")
	}
	if s := hasSymbol(syms, "TITLE", "js_export"); s == nil {
		t.Error("TITLE js_export not extracted")
	}
	// An exported function is a js_func, not also a js_export (no duplication).
	if hasSymbol(syms, "renderHeader", "js_export") != nil {
		t.Error("renderHeader should not also be a js_export")
	}

	if len(refs) != 2 {
		t.Fatalf("want 2 import refs, got %d", len(refs))
	}
	if refs[0].Name != "helper" || refs[0].Spec != "./lib.js" || refs[0].RefKind != "import" {
		t.Errorf("ref[0] = %+v", refs[0])
	}
	if refs[1].Name != "util" { // pre-`as` imported name
		t.Errorf("ref[1].Name = %q, want util", refs[1].Name)
	}
}

func TestExtractSymbols_JSCallRefs(t *testing.T) {
	src := `function setup() {
  render();
  store.save(data);
  const x = helper(1);
}

function render() {
  return 1;
}
`
	_, refs := ExtractSymbols(src, "js")

	callRef := func(name string) *SymbolRef {
		for i := range refs {
			if refs[i].Name == name && refs[i].RefKind == "call" {
				return &refs[i]
			}
		}
		return nil
	}

	// Real usage sites become call edges: the bare call render(), the method call
	// store.save(...), and helper(...) in an initializer.
	if r := callRef("render"); r == nil {
		t.Error("render() call edge not extracted")
	} else if r.Line != 2 {
		t.Errorf("render call line = %d, want 2", r.Line)
	}
	if callRef("save") == nil {
		t.Error("store.save() method call edge not extracted")
	}
	if callRef("helper") == nil {
		t.Error("helper() call edge not extracted")
	}

	// Declaration headers are never call edges: `function render() {` on line 7 and
	// `function setup() {` must not produce a self-referential call to themselves.
	for _, r := range refs {
		if r.RefKind == "call" && (r.Name == "setup" || (r.Name == "render" && r.Line == 7)) {
			t.Errorf("declaration header counted as a call edge: %+v", r)
		}
	}
}

func TestExtractSymbols_CSS(t *testing.T) {
	src := `.header { color: red; }
#nav, .menu {
  display: flex;
}
@media (min-width: 600px) {
  .header { color: blue; }
}
`
	syms, _ := ExtractSymbols(src, "css")
	for _, want := range []string{".header", "#nav", ".menu"} {
		if hasSymbol(syms, want, "css_selector") == nil {
			t.Errorf("selector %q not extracted", want)
		}
	}
	// @media is an at-rule, never a selector symbol.
	if hasSymbol(syms, "@media (min-width: 600px)", "css_selector") != nil {
		t.Error("@media should not be a selector symbol")
	}
}

func TestExtractSymbols_HTML(t *testing.T) {
	src := `<header id="site-header" class="bar dark">
  <nav class="menu"></nav>
</header>`
	syms, _ := ExtractSymbols(src, "html")
	if hasSymbol(syms, "site-header", "html_id") == nil {
		t.Error("site-header html_id not extracted")
	}
	for _, c := range []string{"bar", "dark", "menu"} {
		if hasSymbol(syms, c, "html_class") == nil {
			t.Errorf("class %q not extracted", c)
		}
	}
}
