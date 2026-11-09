package pack

import (
	"strconv"
	"strings"
)

// Section is one addressable EXTENDED section of a pack: a slice the engine does
// NOT inline up front but the model pulls on demand via `manage_guides
// action=recall`. Anchor is derived from the `##` heading (see sectionAnchor) so
// it is stable and quotable.
type Section struct {
	Anchor string `json:"anchor"`
	Title  string `json:"title"`
	When   string `json:"when"` // the `> when:` hint, or "" (falls back to Title)
	Body   string `json:"body"` // the section incl. its `## ` heading
}

// extendedDivider marks the boundary between a pack's CORE body (always inlined
// when active) and its EXTENDED sections (recalled on demand).
const extendedDivider = "<!-- extended -->"

// splitCoreExtended splits a pack body into its CORE text and EXTENDED sections.
// With no divider it returns (whole body, nil). Deterministic: sections follow
// document order and anchors are de-duplicated within the pack.
func splitCoreExtended(body string) (core string, sections []Section) {
	body = strings.TrimSpace(body)
	idx := strings.Index(body, extendedDivider)
	if idx < 0 {
		return body, nil
	}
	core = strings.TrimSpace(body[:idx])
	ext := body[idx+len(extendedDivider):]

	seen := map[string]bool{}
	var cur *Section
	var buf []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Body = strings.TrimSpace(strings.Join(buf, "\n"))
		cur.When = extractWhen(cur.Body)
		sections = append(sections, *cur)
		cur, buf = nil, nil
	}
	for _, line := range strings.Split(ext, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			title := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			cur = &Section{Anchor: uniqueAnchor(sectionAnchor(title), seen), Title: title}
			buf = []string{line}
			continue
		}
		if cur != nil {
			buf = append(buf, line)
		}
	}
	flush()
	return core, sections
}

// uniqueAnchor de-duplicates anchors within one pack by suffixing -2, -3, ...
func uniqueAnchor(base string, seen map[string]bool) string {
	if base == "" {
		base = "section"
	}
	a := base
	for n := 2; seen[a]; n++ {
		a = base + "-" + strconv.Itoa(n)
	}
	seen[a] = true
	return a
}

// extractWhen returns the first `> when: ...` hint in a section body, or "".
func extractWhen(sectionBody string) string {
	for _, line := range strings.Split(sectionBody, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(t), "> when:") {
			if i := strings.Index(t, ":"); i >= 0 {
				return strings.TrimSpace(t[i+1:])
			}
		}
	}
	return ""
}

// sectionAnchor converts a `##` heading into a stable anchor: lowercase, ASCII
// alphanumerics kept, any other run collapsed to a single hyphen, capped at 64.
// (A distinct variant from stringutil.Slug and TaskSlug; see the slug-dedup note.)
func sectionAnchor(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
		if b.Len() >= 64 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// sectionWhen returns a section's "when" hint, falling back to its title.
func sectionWhen(s Section) string {
	if strings.TrimSpace(s.When) != "" {
		return s.When
	}
	return s.Title
}
