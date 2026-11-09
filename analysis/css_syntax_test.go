package analysis

import "testing"

// CSSSyntax judges block-brace balance only, and only for .css paths. Braces
// inside comments and strings are masked out so they never read as structure.
func TestCSSSyntax(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		body      string
		wantGated bool
		wantClean bool
	}{
		{"balanced", "css/app.css", ".a{color:red}.b{margin:0}", true, true},
		{"nested at-rule balanced", "css/app.css", "@media(max-width:600px){.a{color:red}}", true, true},
		{"unclosed block", "css/app.css", ".a{color:red", true, false},
		{"stray close", "css/app.css", ".a{color:red}}", true, false},
		{"brace inside a string is not structural", "css/app.css", `.a{content:"}"}`, true, true},
		{"brace inside a comment is not structural", "css/app.css", ".a{color:red}/* } */", true, true},
		{"empty file is clean", "css/app.css", "", true, true},
		{"non-css path is not gated", "js/app.js", ".a{color:red", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := CSSSyntax(c.path, c.body)
			if r.Gated != c.wantGated {
				t.Fatalf("Gated = %v, want %v", r.Gated, c.wantGated)
			}
			if r.Gated && r.Clean != c.wantClean {
				t.Fatalf("Clean = %v, want %v (detail: %q)", r.Clean, c.wantClean, r.Detail)
			}
		})
	}
}
