package pack

import (
	"regexp"
	"strings"
)

// Conditional prompt text. A stage body or fragment may wrap a block that only
// matters for some projects (the headless rules, the module-split build order, the
// user-flow script) in
//
//	<!-- when:NAME -->
//	...
//	<!-- /when -->
//
// and the caller passes what it knows about the project as Conditions. A block whose
// condition is known false is dropped; one whose condition is unset is kept, so a
// stage that knows nothing about the project (PLAN, LIVE, a test) sees every rule.
// "when:!NAME" inverts. Blocks do not nest. Deterministic, like the rest of Assemble.

var whenBlock = regexp.MustCompile(`(?s)<!-- when:(!?)([a-z_]+) -->\n?(.*?)<!-- /when -->\n?`)

// applyConditions keeps or drops each when-block.
func applyConditions(body string, conds map[string]bool) string {
	return whenBlock.ReplaceAllStringFunc(body, func(block string) string {
		m := whenBlock.FindStringSubmatch(block)
		neg, name, inner := m[1] == "!", m[2], m[3]
		val, known := conds[name]
		if !known || val != neg {
			return inner
		}
		return ""
	})
}

// omitLaws removes the mental model's law sections for the given contract ids: the
// "### <title>" heading and the {{contract:id}} line under it. Used where a law
// cannot apply to the project (the REST envelope in a build with no API).
func omitLaws(body string, ids map[string]bool) string {
	if len(ids) == 0 {
		return body
	}
	for id, omit := range ids {
		if !omit {
			continue
		}
		re := regexp.MustCompile(`(?m)^### [^\n]*\n\{\{contract:` + regexp.QuoteMeta(id) + `\}\}\n*`)
		body = re.ReplaceAllString(body, "")
	}
	return strings.TrimRight(body, "\n") + "\n"
}
