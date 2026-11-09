// Package stringutil holds the canonical small string helpers (rune-safe
// truncation, ASCII slugs). One implementation, so callers cannot diverge.
package stringutil

import (
	"fmt"
	"strings"
	"unicode"
)

// Truncate clips s at maxLen runes (rune-safe) and appends "..." when it
// clips. Byte-wise slicing (s[:max]) is NOT equivalent: it can cut a
// multibyte character in half and emit invalid UTF-8.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// TaskSlug is a checklist task id from free text: lowercased, letters and digits
// kept (any script), every other run collapsed into one hyphen, trimmed, about 48
// characters at most. The engine and the manage_tasks tool both mint of_tasks ids
// with it, so an id the plan wrote and one the agent added compare the same way.
func TaskSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// UniqueTaskSlug returns a checklist-unique task id: the TaskSlug of id, else of
// title, else "task", with -2/-3/... on collision with taken.
func UniqueTaskSlug(id, title string, taken map[string]bool) string {
	base := TaskSlug(id)
	if base == "" {
		base = TaskSlug(title)
	}
	if base == "" {
		base = "task"
	}
	candidate := base
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
	return candidate
}

// Slug turns free-form text into a lowercase ASCII slug: a-z0-9 kept,
// space/underscore/hyphen/dot each become a hyphen (not collapsed, matching
// the historical tenant behavior), everything else dropped, then trimmed and
// capped at maxLen (63 fits a DNS label). maxLen <= 0 means no cap.
func Slug(s string, maxLen int) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '-' || r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if maxLen > 0 && len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}
