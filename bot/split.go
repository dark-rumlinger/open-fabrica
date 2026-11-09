package bot

import (
	"strings"
	"unicode/utf8"
)

// splitMessage breaks text into chunks no longer than max bytes, preferring
// natural boundaries (paragraph, newline, space) when possible. Inputs at or
// under the limit return a single-element slice.
//
// Cuts never land inside an HTML tag ("<...>"), which would both corrupt the
// visible text and make Telegram reject the chunk. A tag that straddles two
// chunks (open in one, close in the next) is still possible; the provider's
// plaintext fallback in sendOne covers that residual case.
func splitMessage(text string, max int) []string {
	if max <= 0 || len(text) <= max {
		return []string{text}
	}

	var out []string
	for len(text) > max {
		cut := max
		// Prefer breaking at the last paragraph boundary before the limit.
		if idx := strings.LastIndex(text[:max], "\n\n"); idx > max/2 {
			cut = idx + 2
		} else if idx := strings.LastIndex(text[:max], "\n"); idx > max/2 {
			cut = idx + 1
		} else if idx := strings.LastIndex(text[:max], " "); idx > max/2 {
			cut = idx + 1
		}
		cut = avoidTagSplit(text, cut)
		// Never cut inside a multibyte character: it would emit invalid UTF-8.
		for cut > 1 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		out = append(out, strings.TrimRight(text[:cut], " \n"))
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// avoidTagSplit pulls the cut point back to just before an unterminated "<" so
// a chunk never ends mid-tag. If the tag opens at the very start (no room to
// back off), the original cut is kept and the plaintext fallback handles it.
func avoidTagSplit(text string, cut int) int {
	if cut <= 0 || cut >= len(text) {
		return cut
	}
	lt := strings.LastIndex(text[:cut], "<")
	gt := strings.LastIndex(text[:cut], ">")
	if lt > gt && lt > 0 {
		return lt
	}
	return cut
}
