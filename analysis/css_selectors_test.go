package analysis

import (
	"fmt"
	"strings"
	"testing"
)

// ExtractStyleSurface reads the names-only design-system surface from a CSS body.
// These tests pin the masked view and the prelude-only class reads, so a token in
// a string/comment or a value like url(.png) is never mistaken for a class.
func TestExtractStyleSurface(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantVars      string
		wantClasses   string
		wantKeyframes string
		wantTruncated bool
	}{
		{
			name:          "normal stylesheet",
			body:          ":root{--color-bg:#fff;--space-3:12px}\n.btn{color:var(--color-bg)}\n.card{padding:var(--space-3)}\n@keyframes fade-in{from{opacity:0}to{opacity:1}}\n.fade{animation:fade-in .2s}",
			wantVars:      "--color-bg,--space-3",
			wantClasses:   "btn,card,fade",
			wantKeyframes: "fade-in",
		},
		{
			name:        "class inside a string is not a class",
			body:        `.real{content:".fake"}`,
			wantClasses: "real",
		},
		{
			name:        "class inside a comment is not a class",
			body:        ".real{color:red}/* .fake-commented {} */",
			wantClasses: "real",
		},
		{
			name:        "var use without declaration is not a token",
			body:        ".btn{color:var(--accent)}",
			wantClasses: "btn",
			wantVars:    "", // --accent is only used, never declared
		},
		{
			name:        "var declaration is a token",
			body:        ":root{--accent:#f00}.btn{color:var(--accent)}",
			wantVars:    "--accent",
			wantClasses: "btn",
		},
		{
			name:        "class nested in @media is extracted",
			body:        "@media (max-width:600px){.mobile-only{display:block}}",
			wantClasses: "mobile-only",
		},
		{
			name:        "url value is not a class",
			body:        ".logo{background:url(brand.png)}",
			wantClasses: "logo",
		},
		{
			name:          "minified equals pretty",
			body:          ":root{--a:1;--b:2}.x{color:var(--a)}.y{color:var(--b)}@keyframes spin{to{transform:rotate(360deg)}}",
			wantVars:      "--a,--b",
			wantClasses:   "x,y",
			wantKeyframes: "spin",
		},
		{
			name:          "vendor-prefixed keyframes dedupe with plain",
			body:          "@-webkit-keyframes spin{}@keyframes spin{}.s{animation:spin 1s}",
			wantClasses:   "s",
			wantKeyframes: "spin",
		},
		{
			name:        "compound and pseudo selectors",
			body:        "a.link:hover{color:blue}.btn.btn--primary{}",
			wantClasses: "link,btn,btn--primary",
		},
		{
			name: "empty body",
			body: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ss := ExtractStyleSurface(c.body)
			if got := strings.Join(ss.Vars, ","); got != c.wantVars {
				t.Errorf("Vars = %q, want %q", got, c.wantVars)
			}
			if got := strings.Join(ss.Classes, ","); got != c.wantClasses {
				t.Errorf("Classes = %q, want %q", got, c.wantClasses)
			}
			if got := strings.Join(ss.Keyframes, ","); got != c.wantKeyframes {
				t.Errorf("Keyframes = %q, want %q", got, c.wantKeyframes)
			}
			if ss.Truncated != c.wantTruncated {
				t.Errorf("Truncated = %v, want %v", ss.Truncated, c.wantTruncated)
			}
		})
	}
}

// A pasted framework/utility build (thousands of classes) must be capped, never
// expanded into the contract: the class list is bounded and Truncated is set.
func TestExtractStyleSurface_FrameworkDumpGuard(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, ".u-%d{margin:%dpx}", i, i)
	}
	ss := ExtractStyleSurface(b.String())
	if !ss.Truncated {
		t.Fatalf("a 500-class body must set Truncated")
	}
	if len(ss.Classes) != styleMaxClasses {
		t.Fatalf("class list = %d, want capped at %d", len(ss.Classes), styleMaxClasses)
	}
}

// The per-list caps bound vars and keyframes too, so no single huge list can
// bloat the contract.
func TestExtractStyleSurface_PerListCaps(t *testing.T) {
	var vars strings.Builder
	vars.WriteString(":root{")
	for i := 0; i < styleMaxVars+20; i++ {
		fmt.Fprintf(&vars, "--v%d:%d;", i, i)
	}
	vars.WriteString("}")
	ss := ExtractStyleSurface(vars.String())
	if len(ss.Vars) != styleMaxVars {
		t.Fatalf("vars = %d, want capped at %d", len(ss.Vars), styleMaxVars)
	}
	if !ss.Truncated {
		t.Fatalf("exceeding the var cap must set Truncated")
	}
}
