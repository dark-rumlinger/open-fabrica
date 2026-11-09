package tools

import "testing"

// FindQueryLog is shared by the engine and the owner chat so both honour find_tool's
// promise that a reworded failed search is refused.
func TestFindQueryLogRefusesParaphrase(t *testing.T) {
	var l FindQueryLog
	if l.Repeat("generate AI images") != "" {
		t.Fatal("an empty log cannot refuse anything")
	}
	l.Record("generate AI images")
	if got := l.Repeat("tool to generate an image"); got != "generate AI images" {
		t.Errorf("a paraphrase must be refused, got %q", got)
	}
	// One shared subject word is not a restated question.
	l.Record("web search")
	if got := l.Repeat("keyword search index"); got != "" {
		t.Errorf("a different question sharing one word must not be refused, got %q", got)
	}
	if res := RepeatSearchResult("x"); !res.Success || res.Note == "" {
		t.Errorf("the refusal must be a successful result with a note, got %+v", res)
	}
}
