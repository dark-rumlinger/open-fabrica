// Package jsonrepair holds the best-effort repairs that turn loose, LLM-emitted
// JSON into something a strict decoder accepts, saving a full re-generation.
// All transforms are string-literal aware: they only rewrite structural tokens
// outside string values, so a value like "True story," is never touched. Every
// transform is a no-op when its trigger byte is absent, so clean JSON passes
// through byte-identical.
package jsonrepair

import (
	"fmt"
	"strings"
)

// Repair applies the full loose-JSON repair chain; safe on already-clean JSON
// (every step is a no-op then). Callers needing the "expected object" guard
// should UnwrapJSON + check the first byte themselves, then call Repair.
func Repair(s string) string {
	s = UnwrapJSON(s)
	s = TrimUnbalancedTrailingBraces(s)
	s = normalizePyLiteralsAndTrailingCommas(s)
	s = EscapeRawControlChars(s)
	return s
}

// Object returns the JSON object in a model reply that should be one: unwrapped
// from a fence, cut from the first '{' to the last '}' when unfenced prose surrounds
// it, then repaired (Repair). For a reply decoded straight into a struct.
func Object(s string) string {
	s = UnwrapJSON(s)
	if i := strings.Index(s, "{"); i > 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	return Repair(s)
}

// UnwrapJSON returns the JSON payload from a model response, tolerating a fence
// around the whole reply and a fenced JSON block buried in narration prose. It
// tries the whole-payload strip first and only recovers an embedded fenced block
// when that leaves prose; otherwise the trimmed input is returned unchanged.
func UnwrapJSON(s string) string {
	s = strings.TrimSpace(s)
	if stripped := StripCodeFence(s); stripped != s {
		return stripped // a fence wrapped the whole reply
	}
	if s == "" || s[0] == '{' || s[0] == '[' {
		return s // already a bare object/array (or empty)
	}
	if inner := ExtractFencedJSON(s); inner != "" {
		return inner
	}
	return s
}

// ExtractFencedJSON returns the contents of the FIRST ```-fenced block embedded
// in prose, or "" when there is no matched fence pair - an unterminated
// (truncated) fence yields "" rather than a valid-looking fragment. The opening
// fence's language tag line is dropped; the first subsequent ``` closes the block.
func ExtractFencedJSON(s string) string {
	open := strings.Index(s, "```")
	if open < 0 {
		return ""
	}
	nl := strings.IndexByte(s[open:], '\n')
	if nl < 0 {
		return "" // fence-open line never terminates
	}
	rest := s[open+nl+1:]
	end := strings.Index(rest, "```")
	if end < 0 {
		return "" // no closing fence
	}
	return strings.TrimSpace(rest[:end])
}

// StripCodeFence drops a single leading/trailing markdown fence pair if one wraps
// the entire payload (```json\n{...}\n``` -> {...}). Only strips a MATCHED pair;
// returns s (trimmed) unchanged when there is no closing fence, so a truncated
// half-fenced payload is not silently turned into "valid-looking" JSON.
func StripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	rest := strings.TrimRight(s[nl+1:], " \t\r\n")
	if !strings.HasSuffix(rest, "```") {
		return s
	}
	return strings.TrimRight(rest[:len(rest)-3], " \t\r\n")
}

// TrimUnbalancedTrailingBraces walks the input tracking string/escape state and
// returns the prefix ending at the bracket/brace that closes the root value;
// junk past that point is dropped. Both `{}` and `[]` are counted so a root
// array is not truncated at its first inner `}`. Input that never balances is
// returned unchanged.
func TrimUnbalancedTrailingBraces(s string) string {
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return s
}

// EscapeRawControlChars escapes raw control bytes (< 0x20) INSIDE a JSON string
// literal into their valid JSON escape, leaving structural whitespace untouched.
// No-op when already well-escaped. Safe over UTF-8: a multi-byte rune's
// continuation bytes are all >= 0x80, so a byte-level scan never splits one.
func EscapeRawControlChars(s string) string {
	// Fast path: nothing to do unless a raw control byte is present.
	hasCtl := false
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 {
			hasCtl = true
			break
		}
	}
	if !hasCtl {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				b.WriteByte(c)
				continue
			}
			switch {
			case c == '\\':
				esc = true
				b.WriteByte(c)
			case c == '"':
				inStr = false
				b.WriteByte(c)
			case c < 0x20:
				switch c {
				case '\n':
					b.WriteString(`\n`)
				case '\r':
					b.WriteString(`\r`)
				case '\t':
					b.WriteString(`\t`)
				case '\b':
					b.WriteString(`\b`)
				case '\f':
					b.WriteString(`\f`)
				default:
					fmt.Fprintf(&b, `\u%04x`, c)
				}
			default:
				b.WriteByte(c)
			}
			continue
		}
		if c == '"' {
			inStr = true
		}
		// Outside strings, raw control bytes are structural whitespace - keep them.
		b.WriteByte(c)
	}
	return b.String()
}

// normalizePyLiteralsAndTrailingCommas rewrites two artifacts json.Unmarshal
// rejects, both string-literal aware:
//   - bare Python literals in value position: True->true, False->false, None->null.
//   - a trailing comma before a closing `}` or `]` (`[1,2,]` -> `[1,2]`).
//
// Outside a string the only legal identifier runs are true/false/null and
// numbers, so replacing an exact True/False/None word is safe.
func normalizePyLiteralsAndTrailingCommas(s string) string {
	// Fast path: nothing to do unless a trigger byte (T/F/N for literals, or a
	// comma for a possible trailing comma) is present.
	if !strings.ContainsAny(s, "TFN,") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inStr, esc := false, false
	for i := 0; i < len(s); {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			i++
			continue
		}
		switch {
		case c == '"':
			inStr = true
			b.WriteByte(c)
			i++
		case isIdentStart(c):
			// Consume the whole identifier run and map the three Python literals.
			j := i + 1
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			switch s[i:j] {
			case "True":
				b.WriteString("true")
			case "False":
				b.WriteString("false")
			case "None":
				b.WriteString("null")
			default:
				b.WriteString(s[i:j])
			}
			i = j
		case c == ',':
			// Drop the comma if the next non-whitespace byte closes a container.
			k := i + 1
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n' || s[k] == '\r') {
				k++
			}
			if k < len(s) && (s[k] == '}' || s[k] == ']') {
				i++ // skip the trailing comma
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
