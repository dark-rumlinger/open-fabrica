package content

import (
	"fmt"
	"strings"
	"testing"
)

// TestNumberLines covers the display-only gutter: correct start offset for both
// full and ranged reads, and the empty-body short-circuit.
func TestNumberLines(t *testing.T) {
	t.Run("from line 1", func(t *testing.T) {
		got := numberLines("a\nb\nc", 1)
		want := fmt.Sprintf("%d| a\n%d| b\n%d| c", 1, 2, 3)
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("ranged read keeps absolute line numbers", func(t *testing.T) {
		// A window that starts at line 745 must number from 745, not from 1 -
		// this is exactly what let the model lose its place before.
		got := numberLines("x\ny", 745)
		want := fmt.Sprintf("%d| x\n%d| y", 745, 746)
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		if got := numberLines("", 1); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})

	t.Run("non-positive start clamps to 1", func(t *testing.T) {
		got := numberLines("only", 0)
		want := fmt.Sprintf("%d| only", 1)
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
}

// TestTrimToByteBudget covers the line-boundary pagination: whole-block
// pass-through, trimming to the last fitting line, and the single-oversized-line
// guarantee that a get never returns an empty page.
func TestTrimToByteBudget(t *testing.T) {
	t.Run("fits whole", func(t *testing.T) {
		in := "aaa\nbbb\nccc"
		out, kept := trimToByteBudget(in, 1000)
		if out != in || kept != 3 {
			t.Fatalf("got (%q, %d) want (%q, 3)", out, kept, in)
		}
	})

	t.Run("trims on a line boundary", func(t *testing.T) {
		// Three 3-char lines joined by newlines = "aaa\nbbb\nccc" (11 bytes).
		// A budget of 7 fits "aaa\nbbb" (7 bytes) but not the trailing "\nccc".
		out, kept := trimToByteBudget("aaa\nbbb\nccc", 7)
		if out != "aaa\nbbb" || kept != 2 {
			t.Fatalf("got (%q, %d) want (%q, 2)", out, kept, "aaa\nbbb")
		}
		if strings.Contains(out, "ccc") {
			t.Fatalf("budget overrun: %q", out)
		}
	})

	t.Run("single oversized line returned whole", func(t *testing.T) {
		long := strings.Repeat("x", 50)
		out, kept := trimToByteBudget(long+"\nnext", 10)
		if out != long || kept != 1 {
			t.Fatalf("got (%q, %d) want oversized line kept whole", out, kept)
		}
	})

	t.Run("non-positive budget passes through", func(t *testing.T) {
		in := "a\nb"
		out, kept := trimToByteBudget(in, 0)
		if out != in || kept != 2 {
			t.Fatalf("got (%q, %d) want (%q, 2)", out, kept, in)
		}
	})
}

// TestPaginationRoundTrip verifies the cursor math the get handler relies on:
// paging through a body with next_from_line leaves no gaps and no overlaps, and
// terminates exactly at total_lines. This is the property that collapses the
// old re-read storm into a clean outline-free sweep when needed.
func TestPaginationRoundTrip(t *testing.T) {
	const total = 40
	lines := make([]string, total)
	for i := range lines {
		// Each line is wide enough that a small budget forces several pages.
		lines[i] = fmt.Sprintf("line-%02d-%s", i+1, strings.Repeat("z", 20))
	}
	body := strings.Join(lines, "\n")

	const budget = 120
	from := 1
	covered := 0
	pages := 0
	for from <= total {
		window := sliceLines(body, from, total)
		numbered := numberLines(window, from)
		_, kept := trimToByteBudget(numbered, budget)
		if kept == 0 {
			t.Fatalf("page from %d returned 0 lines", from)
		}
		effTo := from + kept - 1
		if effTo > total {
			effTo = total
		}
		if effTo != from+kept-1 && effTo != total {
			t.Fatalf("effTo math off: from=%d kept=%d effTo=%d", from, kept, effTo)
		}
		covered += effTo - from + 1
		pages++
		if pages > total+1 {
			t.Fatalf("pagination did not terminate (from=%d)", from)
		}
		from = effTo + 1
	}
	if covered != total {
		t.Fatalf("covered %d lines, want %d (gap or overlap)", covered, total)
	}
	if pages < 2 {
		t.Fatalf("expected multiple pages under budget %d, got %d", budget, pages)
	}
}
