package content

import (
	"strings"
	"testing"
)

// TestApplyPatches_RefusesFuzzyEmptyDelete pins the content-loss guard: an
// empty-replace patch that lands only via the fuzzy matcher - the shape that
// silently truncates a file - is refused and the body is left untouched, while an
// exact-anchored deletion still applies.
func TestApplyPatches_RefusesFuzzyEmptyDelete(t *testing.T) {
	body := "function compute(a, b) {\n  const total = a + b;\n  return total;\n}\n"
	// The search differs from the body on one line, so exact / ws / indent layers
	// all miss and only the fuzzy layer can match; the replace is empty (deletion).
	out, res := ApplyPatches(body, []SearchReplacePatch{{
		Search:  "function compute(a, b) {\n  const total = a + c;\n  return total;\n}",
		Replace: "",
	}})
	if out != body {
		t.Fatalf("guarded deletion must leave the body unchanged, got %q", out)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("guarded patch should not apply, Applied=%v", res.Applied)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "fuzzy") {
		t.Fatalf("expected a fuzzy-deletion refusal warning, got %v", res.Warnings)
	}
	if len(res.Refused) != 1 || len(res.NotFound) != 0 {
		t.Fatalf("a guarded deletion belongs in Refused, not NotFound, got Refused=%v NotFound=%v", res.Refused, res.NotFound)
	}

	// Control: an exact anchor with an empty replace still deletes its span.
	out2, _ := ApplyPatches(body, []SearchReplacePatch{{
		Search: "  const total = a + b;\n", Replace: "",
	}})
	if strings.Contains(out2, "a + b") {
		t.Fatalf("an exact-anchored deletion should still apply, got %q", out2)
	}
}

// A search the model typed with literal backslash-n (two characters) where the
// body has real newlines must still match - otherwise the miss drives the agent
// off patch and into a fragile full-rewrite loop. The replacement is
// unescaped too, so the inserted text gets real newlines, not literal escapes.
func TestApplyPatches_LiteralEscapeSearch(t *testing.T) {
	body := "const a = 1;\nconst b = 2;\nconst c = 3;\n"
	patches := []SearchReplacePatch{{
		Search:  `const b = 2;\nconst c = 3;`, // literal \n, not a newline
		Replace: `const b = 2;\nconst c = 3;\nconst d = 4;`,
	}}
	out, res := ApplyPatches(body, patches)
	if len(res.NotFound) != 0 {
		t.Fatalf("literal-escape search should match a real-newline body, got NotFound=%v", res.NotFound)
	}
	want := "const a = 1;\nconst b = 2;\nconst c = 3;\nconst d = 4;\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if strings.Contains(out, `\n`) {
		t.Fatalf("replacement must carry real newlines, not literal escapes: %q", out)
	}
}

// A search authored with CRLF must still land against an LF-stored body - the
// "patch with \r didn't match - file uses \n" failure this layer prevents.
func TestApplyPatches_CRLFSearchAgainstLFBody(t *testing.T) {
	body := "line one\nline two\nline three\n"
	patches := []SearchReplacePatch{{
		Search:  "line one\r\nline two",
		Replace: "line one\r\nLINE TWO",
	}}
	out, res := ApplyPatches(body, patches)
	if len(res.NotFound) != 0 {
		t.Fatalf("CRLF search should match LF body, got NotFound=%v", res.NotFound)
	}
	want := "line one\nLINE TWO\nline three\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if strings.Contains(out, "\r") {
		t.Fatalf("replacement must not reintroduce CR into an LF body: %q", out)
	}
}

// Trailing-whitespace drift in the anchor must not defeat the match.
func TestApplyPatches_TrailingWhitespaceTolerant(t *testing.T) {
	body := "if (ok) {\n    doThing();\n}\n"
	patches := []SearchReplacePatch{{
		Search:  "if (ok) {   \n    doThing();", // stray trailing spaces
		Replace: "if (ok) {\n    doBetterThing();",
	}}
	out, res := ApplyPatches(body, patches)
	if len(res.NotFound) != 0 {
		t.Fatalf("trailing-ws drift should still match, got NotFound=%v", res.NotFound)
	}
	if !strings.Contains(out, "doBetterThing();") {
		t.Fatalf("replacement not applied: %q", out)
	}
}

// An anchor given with less indentation than the file must match, and the
// replacement must be re-indented to fit so the result stays well-formed.
func TestApplyPatches_IndentationFlexibleReindentsReplacement(t *testing.T) {
	body := "function f() {\n        const a = 1;\n        const b = 2;\n}\n"
	patches := []SearchReplacePatch{{
		Search:  "const a = 1;\nconst b = 2;", // no leading indent
		Replace: "const a = 10;\nconst b = 20;",
	}}
	out, res := ApplyPatches(body, patches)
	if len(res.NotFound) != 0 {
		t.Fatalf("indent-flex should match, got NotFound=%v", res.NotFound)
	}
	want := "function f() {\n        const a = 10;\n        const b = 20;\n}\n"
	if out != want {
		t.Fatalf("re-indent failed:\n got %q\nwant %q", out, want)
	}
}

// A near-miss anchor (a few chars off) lands via the fuzzy layer and is flagged
// with a verify warning.
func TestApplyPatches_FuzzyAcceptsCloseMatchWithWarning(t *testing.T) {
	body := "const greeting = computeGreeting(userName);\n"
	patches := []SearchReplacePatch{{
		Search:  "const greeting = computeGreeting(username);", // userName vs username
		Replace: "const greeting = computeGreeting(displayName);",
	}}
	out, res := ApplyPatches(body, patches)
	if len(res.NotFound) != 0 {
		t.Fatalf("fuzzy should accept a close match, got NotFound=%v", res.NotFound)
	}
	if !strings.Contains(out, "displayName") {
		t.Fatalf("replacement not applied: %q", out)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.ToLower(strings.Join(res.Warnings, " ")), "verify") {
		t.Fatalf("fuzzy match should warn to verify, got %v", res.Warnings)
	}
}

// A search that is genuinely absent must remain not-found, not be force-fit by
// the fuzzy layer.
func TestApplyPatches_FuzzyRejectsDistantMatch(t *testing.T) {
	body := "const greeting = computeGreeting(userName);\n"
	patches := []SearchReplacePatch{{
		Search:  "return databaseConnection.query(sqlStatement);",
		Replace: "whatever",
	}}
	out, res := ApplyPatches(body, patches)
	if out != body {
		t.Fatalf("body must be unchanged for a distant search, got %q", out)
	}
	if len(res.NotFound) != 1 {
		t.Fatalf("expected NotFound for a distant search, got %v / applied %v", res.NotFound, res.Applied)
	}
}

// An exact match must apply cleanly with no tolerant-match warning (the common
// path must stay quiet).
func TestApplyPatches_ExactMatchNoWarning(t *testing.T) {
	body := "alpha\nbeta\ngamma\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{{Search: "beta", Replace: "BETA"}})
	if out != "alpha\nBETA\ngamma\n" {
		t.Fatalf("exact replace failed: %q", out)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("exact match should not warn, got %v", res.Warnings)
	}
}

// TestApplyPatches_LineCountDrift covers a patch_anchor_not_found where
// the model wrote the anchor on ONE line where the file has the same tokens across
// TWO. Layers 3-5 all compare an
// n-line window against an n-line search, so no amount of whitespace tolerance in
// them could ever match it - the line counts differ.
func TestApplyPatches_LineCountDrift(t *testing.T) {
	body := "/* Speech-bubble toast */\n.toast-bubble {\n  position: fixed;\n  bottom: 22px;\n}\n"
	out, res := ApplyPatches(body, []SearchReplacePatch{{
		Search:  ".toast-bubble { position: fixed;",
		Replace: ".toast-bubble {\n  position: absolute;",
	}})
	if len(res.NotFound) != 0 {
		t.Fatalf("line-count drift should still match, NotFound=%v", res.NotFound)
	}
	if !strings.Contains(out, "position: absolute;") {
		t.Fatalf("replacement did not land: %q", out)
	}
	if strings.Contains(out, "position: fixed;") {
		t.Fatalf("original span was not replaced: %q", out)
	}
	// The tolerant match must announce itself: the model has to be able to tell a
	// reformatted landing from a byte-exact one.
	if !strings.Contains(strings.Join(res.Warnings, " "), "whitespace normalized") {
		t.Fatalf("expected a tolerant-match warning, got %v", res.Warnings)
	}
}

// TestApplyPatches_CollapsedMatchRequiresUniqueness: the collapsed layer throws
// away the line structure the model wrote, so it is only safe while the anchor
// still identifies ONE place. With two candidates it must decline rather than
// guess, leaving the decision to fuzzy's margin check.
func TestApplyPatches_CollapsedMatchRequiresUniqueness(t *testing.T) {
	body := ".a {\n  color: red;\n}\n.b {\n  color: red;\n}\n"
	_, res := ApplyPatches(body, []SearchReplacePatch{{
		Search:  "color: red; }",
		Replace: "color: blue; }",
	}})
	if len(res.Applied) != 0 {
		t.Fatalf("an ambiguous collapsed anchor must not apply, Applied=%v", res.Applied)
	}
}

// TestApplyPatches_CollapsedMatchDoesNotRescueRealMismatch pins the boundary: most
// anchor misses are the gate correctly refusing a bad anchor (a signature the model
// had inverted, a span it had never written).
// Whitespace tolerance must not start applying those - a matcher lenient enough to
// land an inverted search/replace is worse than the miss it "fixes".
func TestApplyPatches_CollapsedMatchDoesNotRescueRealMismatch(t *testing.T) {
	body := "function pushLine(name, text, sys) {\n  return 1;\n}\n"
	// Both the search (an extra param the file lacks) and the replace are absent from the
	// body, so this is a genuine miss - not an already-applied idempotent hunk.
	_, res := ApplyPatches(body, []SearchReplacePatch{{
		Search:  "function pushLine(name, text, sys, senderAv) {",
		Replace: "function pushLine(name, text, senderAv) {",
	}})
	if len(res.Applied) != 0 {
		t.Fatalf("an anchor naming a parameter the file does not have must miss, Applied=%v", res.Applied)
	}
	if len(res.AlreadyApplied) != 0 {
		t.Fatalf("neither search nor replace is in the file, so it is not already-applied, got %v", res.AlreadyApplied)
	}
	if len(res.NotFound) != 1 {
		t.Fatalf("expected the anchor in NotFound, got %v", res.NotFound)
	}
}
