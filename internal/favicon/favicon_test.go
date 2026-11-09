package favicon

import (
	"strings"
	"testing"
)

func TestSVGDeterministic(t *testing.T) {
	first, second := SVG("Tip Calc"), SVG("Tip Calc")
	if first != second {
		t.Fatal("favicon not deterministic")
	}
	if !strings.Contains(SVG("Tip Calc"), ">T</text>") {
		t.Fatalf("glyph should be the uppercased initial, got:\n%s", SVG("Tip Calc"))
	}

	// Same title always the same plate.
	plateA, plateB := Plate("Tip Calc"), Plate("Tip Calc")
	if plateA != plateB {
		t.Fatal("plate not deterministic")
	}

	// No alphanumeric character: bullet fallback, never a blank plate.
	if !strings.Contains(SVG("!!!"), "•") {
		t.Fatal("expected bullet glyph fallback")
	}

	// Title with markup is escaped in the aria-label attribute.
	svg := SVG(`x"<&y`)
	if strings.Contains(svg, `aria-label="x"<&y"`) {
		t.Fatal("attribute not escaped")
	}
	if !strings.Contains(svg, "&quot;") || !strings.Contains(svg, "&amp;") {
		t.Fatalf("escapes missing:\n%s", svg)
	}
}

func TestContrastColor(t *testing.T) {
	// Black glyph on light plates, white on dark.
	if ContrastColor("#ffffff") != "#111111" || ContrastColor("#000000") != "#ffffff" {
		t.Fatal("contrast picks wrong glyph colour")
	}
	if ContrastColor("not-a-color") != "#ffffff" {
		t.Fatal("unparseable colour should fall back to white")
	}
}

// The mark wears the project's own primary when the plan committed one. Without it, every
// project's tab icon and installed app icon would come from one fixed eight-colour pool, so a
// project built in oxblood and parchment would still announce itself in Tailwind indigo.
func TestSVGWithPrimary(t *testing.T) {
	const title = "Ledger"

	if got := SVGWithPrimary(title, "#7a1f2b"); !strings.Contains(got, `fill="#7a1f2b"`) {
		t.Errorf("the plan's primary is not the plate colour: %s", got)
	}
	// Shorthand, and a value the planner wrote without the hash.
	if got := SVGWithPrimary(title, "#fc3"); !strings.Contains(got, `fill="#fc3"`) {
		t.Errorf("a #rgb primary was rejected: %s", got)
	}
	if got := SVGWithPrimary(title, " abc123 "); !strings.Contains(got, `fill="#abc123"`) {
		t.Errorf("a bare hex primary was not normalised: %s", got)
	}
	// The glyph still has to read against whatever colour the plan chose.
	if got := SVGWithPrimary(title, "#fdf6e3"); !strings.Contains(got, `fill="#111111"`) {
		t.Errorf("a pale plate kept a white glyph: %s", got)
	}
	// Anything a browser would not accept as a fill falls back rather than shipping a
	// broken attribute: planners annotate colours, and some answer with a name or a var().
	for _, bad := range []string{"", "rebeccapurple", "var(--primary)", "#d8b4b8 (rose quartz)", "#12345"} {
		if got, want := SVGWithPrimary(title, bad), SVG(title); got != want {
			t.Errorf("primary %q did not fall back to the title plate: %s", bad, got)
		}
	}
}
