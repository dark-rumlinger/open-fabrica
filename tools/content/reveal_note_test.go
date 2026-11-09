package content

import (
	"strings"
	"testing"
)

func TestStrandedRevealClasses(t *testing.T) {
	cases := []struct {
		name string
		css  string
		want string
	}{
		{"hidden base shown by a state class", `.reveal{opacity:0;transform:translateY(20px)} .reveal.revealed{opacity:1;transform:none}`, "reveal"},
		{"state class first", `.fade-up{opacity:0} .in.fade-up{opacity:1}`, "fade-up"},
		{"grouped selector", `.reveal{opacity:0} .reveal.in,.reveal.shown{opacity:1}`, "reveal"},
		{"inside a media query", `@media (min-width: 40em){ .reveal{opacity:0} .reveal.in{opacity:1} }`, "reveal"},
		{"visible by default", `.reveal{opacity:1} .reveal.prep{opacity:0} .reveal.prep.in{opacity:1}`, ""},
		{"visible reset on the base", `.reveal{opacity:0} .reveal{opacity:1} .reveal.in{opacity:1}`, ""},
		{"only a reduced-motion reset", `.reveal{opacity:0} .reveal.in{opacity:1} @media (prefers-reduced-motion: reduce){ .reveal{opacity:1} }`, "reveal"},
		{"not a reveal name", `.toast{opacity:0} .toast.show{opacity:1}`, ""},
		{"hidden with no state rule", `.reveal{opacity:0}`, ""},
		{"commented out", `/* .reveal{opacity:0} .reveal.in{opacity:1} */ body{margin:0}`, ""},
	}
	for _, c := range cases {
		got := strings.Join(strandedRevealClasses(c.css), ",")
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRevealNoteOnlyWhenIntroduced(t *testing.T) {
	bad := `.reveal{opacity:0} .reveal.revealed{opacity:1}`
	if note := revealNote("css/global.css", "", bad); !strings.Contains(note, "`.reveal`") || !strings.Contains(note, ".reveal.prep{opacity:0}") {
		t.Fatalf("a new stranded reveal must be reported with the fix, got %q", note)
	}
	if note := revealNote("css/global.css", bad, bad+` body{margin:0}`); note != "" {
		t.Fatalf("an unrelated edit of a stylesheet that already had it must stay quiet, got %q", note)
	}
	if note := revealNote("js/app.js", "", bad); note != "" {
		t.Fatalf("only stylesheets are checked, got %q", note)
	}
	if note := revealNote("css/global.css", bad, `.reveal{opacity:1} .reveal.prep{opacity:0}`); note != "" {
		t.Fatalf("the repaired stylesheet must be quiet, got %q", note)
	}
}
