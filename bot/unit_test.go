package bot

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSplitMessageNeverCutsInsideTag(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("<code>item ")
		b.WriteString(strings.Repeat("x", 18))
		b.WriteString("</code> ")
	}
	text := b.String()

	chunks := splitMessage(text, 100)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 100 {
			t.Errorf("chunk %d exceeds max: %d bytes", i, len(c))
		}
		// A trailing unterminated '<' means we cut inside a tag.
		if lt, gt := strings.LastIndex(c, "<"), strings.LastIndex(c, ">"); lt > gt {
			t.Errorf("chunk %d ends inside a tag: %q", i, c[lt:])
		}
	}
}

func TestSplitMessageShortPassthrough(t *testing.T) {
	if got := splitMessage("hello", 100); len(got) != 1 || got[0] != "hello" {
		t.Errorf("short text should pass through unchanged, got %v", got)
	}
}

func TestHtmlfEscapesArguments(t *testing.T) {
	got := htmlf("Name: %s", "<b>&evil</b>")
	want := "Name: &lt;b&gt;&amp;evil&lt;/b&gt;"
	if got != want {
		t.Errorf("htmlf = %q, want %q", got, want)
	}
}

func TestParseOptionsJSON(t *testing.T) {
	if got := parseOptionsJSON(""); got != nil {
		t.Errorf(`"" -> %v, want nil`, got)
	}
	if got := parseOptionsJSON("[]"); got != nil {
		t.Errorf(`"[]" -> %v, want nil`, got)
	}
	got := parseOptionsJSON(`["a","b"]`)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v", got)
	}
}

func TestRedactToken(t *testing.T) {
	tok := "1234567890:AAH-TESTTOKEN"
	leaky := `Post "https://api.telegram.org/bot1234567890:AAH-TESTTOKEN/getUpdates": dial tcp: timeout`
	if out := redactToken(leaky, tok); strings.Contains(out, tok) {
		t.Errorf("token leaked through redaction: %q", out)
	}
	if redactToken("no token here", "") != "no token here" {
		t.Error("empty token should be a no-op")
	}
}

func TestIsParseError(t *testing.T) {
	if !isParseError(errors.New("telegram API sendMessage ok=false: Bad Request: can't parse entities: ...")) {
		t.Error("parse-entities error should be detected")
	}
	if isParseError(errors.New("telegram getUpdates request failed: timeout")) {
		t.Error("network error should not be a parse error")
	}
}

func TestNextBackoff(t *testing.T) {
	if got := nextBackoff(time.Second); got != 2*time.Second {
		t.Errorf("nextBackoff(1s) = %v, want 2s", got)
	}
	if got := nextBackoff(25 * time.Second); got != 30*time.Second {
		t.Errorf("nextBackoff should cap at 30s, got %v", got)
	}
}
