package analysis

import (
	"regexp"
	"strings"
)

// classUseRe anchors on a `class=` attribute or a `className=` assignment and
// captures ONLY the static leading run of class tokens after the opening quote
// (letters, digits, _, -, and spaces). The capture stops at the first character
// that cannot belong to a static class list - a quote, `$`, `{`, `+`, etc. - so a
// dynamic value contributes just its static prefix and never a fragment of the
// surrounding expression:
//
//	class="pill-btn submit"            -> pill-btn, submit
//	class="act ${liked?'liked':''}"    -> act
//	'<a class="nav-item' + (on?...)    -> nav-item
//	b.className = 'avatar sm'          -> avatar, sm
//
// The leading boundary set (start, whitespace, quote, brace/paren, `>`, `.`)
// admits the DOM form `el.className=` while rejecting look-alikes such as
// `data-class=` (preceded by `-`). One scan reads both static HTML and the markup
// embedded in JS template/concat strings without a full parse of either.
var classUseRe = regexp.MustCompile(
	"(?:^|[\\s\"'`{(>.])(?:className|class)\\s*=\\s*[\"'`]([A-Za-z0-9_ -]*)")

// ClassGroupsUsed returns one entry per class= / className= occurrence: the static
// class tokens that share a single element. Grouping is what lets a caller judge
// whether an element is styled - an element is unstyled only when NONE of its
// classes has a rule, so a behavior-only hook class (`a-like` on the button
// `class="act a-like"`) must be read alongside the styling class it rides with,
// never flagged in isolation. Tokens within a group are deduplicated; a group with
// no static tokens (a fully dynamic value) is dropped.
func ClassGroupsUsed(body string) [][]string {
	if body == "" {
		return nil
	}
	var groups [][]string
	for _, m := range classUseRe.FindAllStringSubmatch(body, -1) {
		seen := map[string]bool{}
		var g []string
		for _, tok := range strings.Fields(m[1]) {
			if seen[tok] {
				continue
			}
			seen[tok] = true
			g = append(g, tok)
		}
		if len(g) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}
