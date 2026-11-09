package analysis

// Shared executable-inline-<script> extraction. The save-time syntax gate and the
// build-time validators need the same answer to "which <script> blocks are
// executable, and where does each begin"; one definition keeps them from drifting.

import (
	"regexp"
	"strings"
)

var (
	// Inline <script>...</script> with its attributes (group 1) and inner code
	// (group 2). (?is): case-insensitive, dot matches newline; the inner is
	// non-greedy so each block stops at its own </script>.
	reInlineScript = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	// The type attribute value, used to tell an executable script from a data block.
	reScriptType = regexp.MustCompile(`type\s*=\s*['"]?([^'">\s]+)`)
)

// InlineScript is one executable inline <script> block: its inner code and the
// 1-based line in the document where that code begins, so a parse error in the
// block can be reported at a document-relative line.
type InlineScript struct {
	Code      string
	StartLine int
}

// InlineScripts returns every executable inline <script> block in an HTML body,
// in document order. External (src=) scripts and data blocks (application/json,
// importmap, application/ld+json) are skipped - their contents are not run as JS.
func InlineScripts(htmlBody string) []InlineScript {
	var out []InlineScript
	for _, m := range reInlineScript.FindAllStringSubmatchIndex(htmlBody, -1) {
		attrs := htmlBody[m[2]:m[3]]
		if !isExecutableScript(attrs) {
			continue
		}
		innerStart := m[4]
		out = append(out, InlineScript{
			Code:      htmlBody[m[4]:m[5]],
			StartLine: 1 + strings.Count(htmlBody[:innerStart], "\n"),
		})
	}
	return out
}

// isExecutableScript reports whether a <script> tag's attributes denote a script
// the browser executes (so its code is real JS to parse). External (src=) scripts
// and data blocks (application/ld+json, importmap, application/json) are not.
func isExecutableScript(attrs string) bool {
	la := strings.ToLower(attrs)
	if strings.Contains(la, "src=") {
		return false
	}
	if m := reScriptType.FindStringSubmatch(la); m != nil {
		switch strings.TrimSpace(m[1]) {
		case "", "module", "text/javascript", "application/javascript":
			return true
		default:
			return false
		}
	}
	return true // no type attribute => classic executable script
}
