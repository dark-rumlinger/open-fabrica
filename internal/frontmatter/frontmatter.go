// Package frontmatter holds the shared `---` front-matter primitives used by
// the contracts and pack parsers. One home for the split/list grammar, so the
// two parsers cannot drift apart; each package keeps its own key switch.
package frontmatter

import (
	"fmt"
	"strings"
)

// Split separates a "---\nkey: value...\n---\n\nbody" document into its raw
// front-matter header and its trimmed body. CRLF line endings are normalized
// to LF first, so the header lines and body are stable across checkouts.
func Split(raw string) (header, body string, err error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(raw, "---\n") {
		return "", "", fmt.Errorf("missing front-matter")
	}
	rest := raw[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("unterminated front-matter")
	}
	header = rest[:end]
	body = strings.TrimSpace(strings.TrimPrefix(rest[end+4:], "\n"))
	return header, body, nil
}

// SplitList parses a `[a, b, c]` or `a, b, c` front-matter list. A value may
// itself contain a colon (e.g. a `type:'chat'` probe) - only the first colon on
// a header line is the key separator, so the rest survives intact.
func SplitList(val string) []string {
	val = strings.Trim(val, "[]")
	var out []string
	for _, t := range strings.Split(val, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}
