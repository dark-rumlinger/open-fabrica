package content

import "testing"

// A tolerant (line-window) match must edit the window it located, not an earlier
// mid-line occurrence of the same text.
func TestTolerantMatchEditsLocatedWindow(t *testing.T) {
	body := "a\nbc\nmid\na\nb\nend"
	out, res := ApplyPatches(body, []SearchReplacePatch{{Search: "a\nb  ", Replace: "X"}})
	if len(res.Applied) != 1 {
		t.Fatalf("expected the patch to apply, got %+v", res)
	}
	if want := "a\nbc\nmid\nX\nend"; out != want {
		t.Fatalf("edited the wrong place:\n got %q\nwant %q", out, want)
	}
}

// A count-limited regex replacement keeps context assertions and group expansion.
func TestRegexReplaceCountKeepsContext(t *testing.T) {
	out, n, err := ApplyRegexReplacements("xfoo yfoo", []RegexReplacement{{Pattern: `\Bfoo`, Replace: "bar", Count: 1}})
	if err != nil || n != 1 || out != "xbar yfoo" {
		t.Fatalf("got %q n=%d err=%v", out, n, err)
	}
	out, n, err = ApplyRegexReplacements("a1 a2 a3", []RegexReplacement{{Pattern: `a(\d)`, Replace: "b${1}", Count: 2}})
	if err != nil || n != 2 || out != "b1 b2 a3" {
		t.Fatalf("got %q n=%d err=%v", out, n, err)
	}
}

// A replacement that carries a quoted \n escape must stay verbatim even when the
// search only matched after its own escapes were read as newlines.
func TestEscapedSearchKeepsQuotedEscapeInReplace(t *testing.T) {
	body := "line1\nline2\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{{Search: `line1\nline2`, Replace: `s.split('\n')`}})
	if len(res.Applied) != 1 {
		t.Fatalf("expected apply, got %+v", res)
	}
	if want := "s.split('\\n')\n"; out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	out, _ = ApplyPatches(body, []SearchReplacePatch{{Search: `line1\nline2`, Replace: `a\nb`}})
	if want := "a\nb\n"; out != want {
		t.Fatalf("plain escapes should still unescape: got %q want %q", out, want)
	}
}
