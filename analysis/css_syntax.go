package analysis

// Shared CSS structural check, the stylesheet counterpart of JSSyntax. It checks
// ONE thing on purpose: block-brace balance. An unbalanced `{`/`}` silently drops
// every rule after it with no browser error; property/value typos are tolerated.

import (
	"fmt"
	"path"
	"strings"
)

// CSSSyntaxReport mirrors JSSyntaxReport: Gated is false for any non-CSS path
// (callers skip the check then); when Gated, Clean reports whether the block
// braces balance and Detail describes the first imbalance.
type CSSSyntaxReport struct {
	Gated  bool
	Clean  bool
	Detail string
}

// CSSSyntax reports whether body's block braces balance. Braces inside comments
// and strings are masked out first (maskCSS), so `content: "}"` never reads as a
// stray close. A non-CSS path returns {Gated: false}.
func CSSSyntax(p, body string) CSSSyntaxReport {
	if strings.ToLower(path.Ext(p)) != ".css" {
		return CSSSyntaxReport{}
	}
	masked := maskCSS(body)
	depth := 0
	line := 1
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '\n':
			line++
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return CSSSyntaxReport{
					Gated:  true,
					Detail: fmt.Sprintf("unexpected '}' with no matching '{' (line %d)", line),
				}
			}
		}
	}
	if depth > 0 {
		return CSSSyntaxReport{
			Gated:  true,
			Detail: fmt.Sprintf("%d unclosed '{' block(s) - a missing '}' drops every rule after it", depth),
		}
	}
	return CSSSyntaxReport{Gated: true, Clean: true}
}
