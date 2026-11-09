package analysis

import "testing"

func TestInlineScripts_SkipsNonExecutable(t *testing.T) {
	body := `<!doctype html>
<script src="/files/app.js"></script>
<script type="application/json">{"a":1}</script>
<script type="importmap">{"imports":{}}</script>
<script type="application/ld+json">{"@context":"x"}</script>
<script type="text/babel">const skip = 1;</script>`
	if got := InlineScripts(body); len(got) != 0 {
		t.Fatalf("expected no executable scripts, got %d: %+v", len(got), got)
	}
}

func TestInlineScripts_AcceptsExecutableTypes(t *testing.T) {
	body := `<script>classic();</script>
<script type="module">mod();</script>
<script type="text/javascript">tj();</script>
<script type="application/javascript">aj();</script>`
	got := InlineScripts(body)
	if len(got) != 4 {
		t.Fatalf("expected 4 executable scripts, got %d: %+v", len(got), got)
	}
	wantCode := []string{"classic();", "mod();", "tj();", "aj();"}
	for i, w := range wantCode {
		if got[i].Code != w {
			t.Errorf("block %d code = %q, want %q", i, got[i].Code, w)
		}
	}
}

func TestInlineScripts_StartLines(t *testing.T) {
	// Block one's code begins on line 2; block two's on line 6 (1-based).
	body := "<div>head</div>\n" + // line 1
		"<script>\n" + // <script> opens on line 2, inner begins right after the '>' on line 2
		"  a();\n" + // line 3
		"</script>\n" + // line 4
		"<p>mid</p>\n" + // line 5
		"<script>b();</script>" // line 6
	got := InlineScripts(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 scripts, got %d: %+v", len(got), got)
	}
	if got[0].StartLine != 2 {
		t.Errorf("block 0 StartLine = %d, want 2", got[0].StartLine)
	}
	if got[1].StartLine != 6 {
		t.Errorf("block 1 StartLine = %d, want 6", got[1].StartLine)
	}
	// The start line is the anchor a syntax error offsets against: a parse error on
	// the second inner line of block 0 should resolve to document line 3.
	if r := JSSyntax("inline.js", got[0].Code); !r.Gated {
		t.Fatal("block 0 code should be gated as JS")
	}
}

func TestInlineScripts_None(t *testing.T) {
	if got := InlineScripts("<p>no scripts here</p>"); len(got) != 0 {
		t.Fatalf("expected no scripts, got %d", len(got))
	}
}

func TestIsExecutableScript(t *testing.T) {
	cases := []struct {
		attrs string
		want  bool
	}{
		{"", true},
		{" defer", true},
		{` type="module"`, true},
		{` type="text/javascript"`, true},
		{` type="application/javascript"`, true},
		{` type="application/json"`, false},
		{` type="importmap"`, false},
		{` src="/files/x.js"`, false},
		{` type="module" src="/files/x.js"`, false},
	}
	for _, c := range cases {
		if got := isExecutableScript(c.attrs); got != c.want {
			t.Errorf("isExecutableScript(%q) = %v, want %v", c.attrs, got, c.want)
		}
	}
}
