package content

import (
	"strings"
	"testing"
)

func numbered(line int, text string) string { return numberLines(text, line) }

// mkLines returns n lines "L1".."Ln" joined by newlines.
func mkLines(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = "L" + itoa(i+1)
	}
	return strings.Join(out, "\n")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestChangedLineRanges(t *testing.T) {
	t.Run("identical bodies", func(t *testing.T) {
		if r := changedLineRanges("a\nb", "a\nb"); r != nil {
			t.Fatalf("expected nil, got %v", r)
		}
	})

	t.Run("single line replace", func(t *testing.T) {
		old := "a\nb\nc"
		neu := "a\nB\nc"
		r := changedLineRanges(old, neu)
		if len(r) != 1 || r[0] != [2]int{2, 2} {
			t.Fatalf("got %v want [[2 2]]", r)
		}
	})

	t.Run("two separated changes give two ranges", func(t *testing.T) {
		old := mkLines(20)
		lines := strings.Split(old, "\n")
		lines[2] = "X"  // line 3
		lines[15] = "Y" // line 16
		neu := strings.Join(lines, "\n")
		r := changedLineRanges(old, neu)
		if len(r) != 2 {
			t.Fatalf("expected 2 ranges, got %v", r)
		}
		if r[0] != [2]int{3, 3} || r[1] != [2]int{16, 16} {
			t.Fatalf("got %v", r)
		}
	})
}

func TestEchoChangedRegion(t *testing.T) {
	t.Run("no ranges returns empty", func(t *testing.T) {
		if s, trunc, spans := echoChangedRegion("a\nb", nil, 4, 4000); s != "" || trunc || spans != nil {
			t.Fatalf("got %q trunc=%v spans=%v", s, trunc, spans)
		}
	})

	t.Run("single change echoes window with post-edit numbers", func(t *testing.T) {
		neu := mkLines(10) // L1..L10
		// pretend line 5 changed
		s, trunc, spans := echoChangedRegion(neu, [][2]int{{5, 5}}, 2, 4000)
		if trunc {
			t.Fatalf("did not expect truncation")
		}
		// context 2 => rendered span is lines 3..7, reported for residence.
		if len(spans) != 1 || spans[0] != [2]int{3, 7} {
			t.Fatalf("expected rendered span [[3 7]], got %v", spans)
		}
		// context 2 => lines 3..7
		if !strings.Contains(s, numbered(3, "L3")) {
			t.Fatalf("missing line 3 context:\n%s", s)
		}
		if !strings.Contains(s, numbered(7, "L7")) {
			t.Fatalf("missing line 7 context:\n%s", s)
		}
		if strings.Contains(s, numbered(2, "L2")) || strings.Contains(s, numbered(8, "L8")) {
			t.Fatalf("window leaked beyond context:\n%s", s)
		}
	})

	t.Run("multi hunk gets a divider", func(t *testing.T) {
		neu := mkLines(40)
		s, _, _ := echoChangedRegion(neu, [][2]int{{3, 3}, {30, 30}}, 2, 4000)
		if !strings.Contains(s, "...") {
			t.Fatalf("expected elided divider between hunks:\n%s", s)
		}
		if !strings.Contains(s, numbered(3, "L3")) || !strings.Contains(s, numbered(30, "L30")) {
			t.Fatalf("missing one of the hunks:\n%s", s)
		}
	})

	t.Run("near EOF clamps cleanly", func(t *testing.T) {
		neu := mkLines(5)
		s, trunc, _ := echoChangedRegion(neu, [][2]int{{5, 5}}, 4, 4000)
		if trunc {
			t.Fatalf("unexpected truncation")
		}
		// context 4 from line 5 would be 1..9, clamp to 1..5.
		if !strings.Contains(s, numbered(1, "L1")) || !strings.Contains(s, numbered(5, "L5")) {
			t.Fatalf("clamp wrong:\n%s", s)
		}
	})

	t.Run("huge change trips truncation under budget", func(t *testing.T) {
		neu := mkLines(2000)
		s, trunc, spans := echoChangedRegion(neu, [][2]int{{1, 2000}}, 4, 4000)
		if !trunc {
			t.Fatalf("expected truncation for a 2000-line change")
		}
		if len(s) > 4000 {
			t.Fatalf("echo exceeded budget: %d bytes", len(s))
		}
		// The single span did not render whole, so it must NOT be reported as
		// resident - else a later get of those lines would be wrongly stubbed.
		if len(spans) != 0 {
			t.Fatalf("expected no fully-rendered spans under truncation, got %v", spans)
		}
	})
}
