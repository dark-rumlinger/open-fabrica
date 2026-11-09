// Package favicon synthesizes a deterministic, project-matched favicon SVG from
// a title, with no LLM token spend. The single source shared by the build
// finalizer and the enable_pwa action, so favicon and PWA app icon are the same mark.
package favicon

import (
	"fmt"
	"strings"
	"unicode"
)

// SVG renders a deterministic 32x32 favicon for a project title: the title's
// initial on a rounded-rect plate, the glyph in whichever of black/white
// contrasts better. Pure function of its input, so a rebuild produces
// byte-identical output and the two callers never disagree on the mark.
func SVG(title string) string {
	return svgWithPlate(title, Plate(title))
}

// SVGWithPrimary renders the mark on the project's OWN primary colour when the plan
// committed one, falling back to the title-hashed plate. A generated project whose
// palette is its identity should not wear a colour from a fixed pool: the tab icon and
// the installed app icon were the last place every project still looked alike. A value
// that is not a plain #rgb/#rrggbb hex (an annotated one, a var(), a name) falls back.
func SVGWithPrimary(title, primary string) string {
	p := strings.TrimSpace(primary)
	if _, _, _, ok := parseHexColor(p); !ok {
		return SVG(title)
	}
	if !strings.HasPrefix(p, "#") {
		p = "#" + p
	}
	return svgWithPlate(title, p)
}

// svgWithPlate renders the favicon on an explicit plate colour. An empty plate
// falls back to neutral indigo.
func svgWithPlate(title, plate string) string {
	if plate == "" {
		plate = "#4f46e5"
	}
	glyph := ContrastColor(plate)
	letter := glyphFor(title)

	// y=22 baselines a 20px-cap letter near the optical centre of the 32px box.
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32" role="img" aria-label="%s">`+
		`<rect width="32" height="32" rx="7" fill="%s"/>`+
		`<text x="16" y="22" text-anchor="middle" font-family="system-ui,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif" font-size="20" font-weight="700" fill="%s">%s</text>`+
		`</svg>`,
		attrEscape(title), plate, glyph, letter)
}

// glyphFor picks the character drawn on the plate: the first letter or digit of
// the title, uppercased; a bullet when none exists.
func glyphFor(title string) string {
	for _, r := range title {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return textEscape(string(unicode.ToUpper(r)))
		}
	}
	return "•"
}

// Plate picks a deterministic plate colour from the title.
func Plate(title string) string {
	palette := []string{"#4f46e5", "#0ea5e9", "#10b981", "#f59e0b", "#ef4444", "#8b5cf6", "#ec4899", "#14b8a6"}
	if strings.TrimSpace(title) == "" {
		return palette[0]
	}
	var h uint32
	for _, r := range title {
		h = h*31 + uint32(r)
	}
	return palette[h%uint32(len(palette))]
}

// ContrastColor returns "#ffffff" or "#111111", whichever reads better against
// the given fill; white for unparseable colours.
func ContrastColor(fill string) string {
	r, g, b, ok := parseHexColor(fill)
	if !ok {
		return "#ffffff"
	}
	lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	if lum > 150 {
		return "#111111"
	}
	return "#ffffff"
}

// parseHexColor parses #rgb / #rrggbb into 8-bit channels.
func parseHexColor(s string) (r, g, b uint8, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	switch len(s) {
	case 3:
		hi := func(c byte) (uint8, bool) { v, ok := hexNibble(c); return v*16 + v, ok }
		r, ok1 := hi(s[0])
		g, ok2 := hi(s[1])
		b, ok3 := hi(s[2])
		return r, g, b, ok1 && ok2 && ok3
	case 6:
		byteAt := func(i int) (uint8, bool) {
			hi, ok1 := hexNibble(s[i])
			lo, ok2 := hexNibble(s[i+1])
			return hi*16 + lo, ok1 && ok2
		}
		r, ok1 := byteAt(0)
		g, ok2 := byteAt(2)
		b, ok3 := byteAt(4)
		return r, g, b, ok1 && ok2 && ok3
	}
	return 0, 0, 0, false
}

func hexNibble(c byte) (uint8, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// attrEscape escapes a string for a double-quoted SVG attribute.
func attrEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// textEscape escapes a string for SVG element text content.
func textEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
