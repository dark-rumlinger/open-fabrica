// Package sqlguard holds the shared SQL-safety checks used wherever the platform
// executes operator- or AI-authored SQL, so the leading-verb and stacked-statement
// logic cannot drift into subtly different, individually-weaker checks.
package sqlguard

import (
	"fmt"
	"strings"
)

// CheckAction validates one stored SQL action (a trigger's run_sql, a job's run_sql, a
// schedule's sql action) the same way for every caller: present, free of {{...}}
// templating (values go through `params` bound to ? placeholders), a single
// statement, and an EffectiveVerb in allowed, so a WITH-wrapped write is judged by
// the write it wraps. Returns that verb.
func CheckAction(q string, allowed ...string) (string, error) {
	if strings.TrimSpace(q) == "" {
		return "", fmt.Errorf("'sql' is required")
	}
	if strings.Contains(q, "{{") {
		return "", fmt.Errorf("'{{...}}' templating is not allowed in 'sql'; pass values via 'params' and use ? placeholders")
	}
	if HasStackedStatements(q) {
		return "", fmt.Errorf("multiple statements are not allowed")
	}
	verb := EffectiveVerb(q)
	for _, a := range allowed {
		if verb == a {
			return verb, nil
		}
	}
	return "", fmt.Errorf("only %s statements are allowed", strings.Join(allowed, "/"))
}

// HasStackedStatements reports whether q contains a statement separator that
// begins a second statement. A single trailing ';' is tolerated; a ';' inside a
// quoted literal/identifier or a comment is ignored. Deliberately biased toward
// false positives, never false negatives: an unbalanced quote swallows the rest
// of the input, while a real ';' outside any quoted region is always seen.
func HasStackedStatements(q string) bool {
	rs := []rune(q)
	n := len(rs)
	for i := 0; i < n; i++ {
		switch c := rs[i]; {
		case c == '\'' || c == '"' || c == '`':
			// String literal ('...'/"...") or backtick-quoted identifier. All
			// three use a doubled delimiter as the in-literal escape.
			quote := c
			i++
			for i < n {
				if rs[i] == quote {
					// A doubled quote is an escaped quote: stay in the literal.
					if i+1 < n && rs[i+1] == quote {
						i += 2
						continue
					}
					break // closing quote; outer loop's i++ steps past it
				}
				i++
			}
		case c == '[':
			// SQLite [bracket]-quoted identifier: no escape mechanism, the first
			// ']' closes it.
			i++
			for i < n && rs[i] != ']' {
				i++
			}
		case c == '-' && i+1 < n && rs[i+1] == '-':
			for i < n && rs[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && rs[i+1] == '*':
			i += 2
			for i+1 < n && !(rs[i] == '*' && rs[i+1] == '/') {
				i++
			}
			i++ // consume the closing '/'
		case c == ';':
			// A ';' begins a second statement only if real SQL follows it.
			// Trailing whitespace and comments after the final ';' are tolerated
			// (they are not another statement), matching the doc contract.
			if stripLeadingNoise(string(rs[i+1:])) != "" {
				return true
			}
		}
	}
	return false
}

// LeadingVerb returns the upper-cased first SQL token of q after stripping
// leading whitespace and -- / /* */ comments, or "" if there is none. Use it to
// match a statement against a verb allowlist without a leading comment or
// whitespace slipping an unintended verb past the check.
func LeadingVerb(q string) string {
	s := stripLeadingNoise(q)
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

// EffectiveVerb returns the upper-cased verb of q's MAIN statement: the leading
// verb for an ordinary statement, and for a `WITH ...` CTE the verb of the
// statement that FOLLOWS the CTE definitions. A `WITH ... SELECT` (e.g. a recursive
// sequence generator) is a READ and returns "SELECT"; a `WITH ... INSERT/UPDATE/
// DELETE` is a WRITE and returns that verb. A read-only gate must key on this, not
// the literal first token - otherwise it rejects a valid read-only CTE (leading
// "WITH") while a WITH-wrapped write could slip a first-token check. Returns "" when
// no verb is found (e.g. a WITH whose main statement is missing/unbalanced).
func EffectiveVerb(q string) string {
	v := LeadingVerb(q)
	if v != "WITH" {
		return v
	}
	rs := []rune(q)
	n := len(rs)
	depth := 0
	afterAS := false // an "AS" was seen at depth 0; the next ()-group is the CTE body
	for i := 0; i < n; {
		switch c := rs[i]; {
		case c == '\'' || c == '"' || c == '`':
			i = skipQuoted(rs, i, c)
		case c == '[':
			i++
			for i < n && rs[i] != ']' {
				i++
			}
			i++
		case c == '-' && i+1 < n && rs[i+1] == '-':
			for i < n && rs[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && rs[i+1] == '*':
			i += 2
			for i+1 < n && !(rs[i] == '*' && rs[i+1] == '/') {
				i++
			}
			i += 2
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
			if depth == 0 && afterAS {
				// Closed a CTE body. The next real token is ',' (another CTE) or the
				// main statement.
				j := i
				for j < n && (rs[j] == ' ' || rs[j] == '\t' || rs[j] == '\r' || rs[j] == '\n') {
					j++
				}
				if j < n && rs[j] == ',' {
					afterAS = false
					i = j + 1
					continue
				}
				return LeadingVerb(string(rs[j:]))
			}
		case depth == 0 && isIdentRune(c):
			start := i
			for i < n && isIdentRune(rs[i]) {
				i++
			}
			if strings.EqualFold(string(rs[start:i]), "AS") {
				afterAS = true // NOT/MATERIALIZED may follow before the body ()
			}
		default:
			i++
		}
	}
	return ""
}

// skipQuoted returns the index just past the string/identifier literal opened by
// quote at rs[i]. A doubled quote is the in-literal escape.
func skipQuoted(rs []rune, i int, quote rune) int {
	n := len(rs)
	i++ // past the opening quote
	for i < n {
		if rs[i] == quote {
			if i+1 < n && rs[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

// isIdentRune reports whether c can be part of a SQL identifier/keyword token.
func isIdentRune(c rune) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// stripLeadingNoise removes leading whitespace and -- / /* */ comments,
// returning the remainder that begins the first real statement token.
func stripLeadingNoise(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"):
			if idx := strings.IndexByte(s, '\n'); idx >= 0 {
				s = s[idx+1:]
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if idx := strings.Index(s, "*/"); idx >= 0 {
				s = s[idx+2:]
				continue
			}
			return ""
		}
		return s
	}
}
