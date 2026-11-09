package data

import (
	"fmt"
	"regexp"
	"strings"
)

// Structural safety checks for the two pieces of raw SQL parseColumnDef accepts: a
// column `check` expression and a `default` literal. Both are interpolated into
// CREATE TABLE / ALTER TABLE text by columnSQL, and the driver executes every
// statement in the string it is given, so a stray ';' or an unbalanced paren can
// close the generated statement and start another. The `of_` system tables are
// reserved on every other path (store.IsSystemTable at create/alter, the
// systemTableRef guard on raw SELECTs in data_exec.go); these checks keep an authored
// constraint from composing SQL that reaches them anyway.
//
// The checks are deliberately structural rather than a grammar allowlist. Legitimate
// constraints contain string literals, function calls, and nested parens
// ("discipline IN ('Branding','Editorial')", "length(name) > 0"), so an operator
// allowlist would reject real schemas. The rule enforced instead is self-containment:
// a fragment must close every quote and paren it opens and introduce no statement
// separator and no comment. That admits every shape normalizeCheckExpr handles while
// making an early close impossible.
//
// Statement-level stacking on assembled DDL is checked separately at the Exec sites
// via sqlguard.HasStackedStatements: these functions guard the fragment, that guards
// the composed statement.

// numericDefaultRe matches a bare SQL numeric literal: optional sign, decimal or
// hex, optional exponent. It is what a numeric DEFAULT is allowed to be, because
// quoteDefaultValue emits those unquoted. It accepts Go's own %d/%g rendering of
// a JSON number (including "1e+20"), so a default supplied as a JSON number is
// never rejected.
var numericDefaultRe = regexp.MustCompile(`^[+-]?(0[xX][0-9a-fA-F]+|(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?)$`)

// defaultKeywords are the bare SQL keywords a DEFAULT may be, emitted verbatim.
var defaultKeywords = map[string]bool{
	"NULL": true, "CURRENT_TIMESTAMP": true, "CURRENT_TIME": true,
	"CURRENT_DATE": true, "TRUE": true, "FALSE": true,
}

// skipQuoted returns the index of the closing quote for the region opened by
// s[start] == q, treating a doubled quote as an escaped quote rather than the
// close - the same rule SQLite applies. ok is false if the region is never
// closed.
func skipQuoted(s string, start int, q byte) (end int, ok bool) {
	for i := start + 1; i < len(s); i++ {
		if s[i] != q {
			continue
		}
		if i+1 < len(s) && s[i+1] == q {
			i++ // doubled: an escaped quote, keep scanning
			continue
		}
		return i, true
	}
	return 0, false
}

// scanSQLFragment reports the first structural reason s is unsafe to interpolate
// into a larger statement, or nil if it is self-contained. Quoted regions are
// skipped wholesale, so a ';', '--' or paren inside a string literal is content
// and not a finding ("IN (';')" is fine).
//
// The scan is biased toward rejection: an unterminated quote is an error rather
// than a silent swallow of the remainder, so a fragment can never be accepted on
// the strength of a region the scanner failed to parse.
func scanSQLFragment(s string) error {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"', '`':
			end, ok := skipQuoted(s, i, c)
			if !ok {
				return fmt.Errorf("unterminated %c-quoted string", c)
			}
			i = end
		case '[':
			j := strings.IndexByte(s[i:], ']')
			if j < 0 {
				return fmt.Errorf("unterminated [ identifier")
			}
			i += j
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return fmt.Errorf("unbalanced ')': closes a parenthesis that was never opened")
			}
		case ';':
			return fmt.Errorf("';' is not allowed: this is a single expression, not a statement")
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				return fmt.Errorf("'--' comment is not allowed")
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '*' {
				return fmt.Errorf("'/*' comment is not allowed")
			}
		}
	}
	if depth > 0 {
		return fmt.Errorf("unbalanced '(': %d parenthesis left open", depth)
	}
	return nil
}

// validateCheckExpr rejects a column `check` expression that is not a
// self-contained SQL expression. The message names the offending shape and
// restates the accepted one, so a rejection can be fixed on the first attempt
// rather than by guessing.
func validateCheckExpr(col, expr string) error {
	if err := scanSQLFragment(expr); err != nil {
		return fmt.Errorf(
			"column %s: invalid check %q: %w. A check is one self-contained boolean expression - "+
				"either a bare fragment (\"> 0\", \"IN ('a','b')\") or a full predicate (\"length(name) > 0\")",
			col, expr, err)
	}
	return nil
}

// validateDefaultLiteral rejects a `default` that quoteDefaultValue would emit
// unquoted or pass through verbatim without it being a safe literal. It guards
// the two passthrough branches in that function:
//
//	already-quoted ("'draft'")   - must be exactly one closed literal and nothing
//	                               after it, or the trailing text lands outside
//	                               the quotes in the emitted DDL;
//	numeric column (INTEGER/...) - must be a bare numeric literal, because the
//	                               value is emitted with no quoting at all.
//
// A bare value on a TEXT column needs no check: quoteDefaultValue escapes and
// quotes it, so it is safe by construction.
func validateDefaultLiteral(col, val, sqlType string) error {
	if val == "" {
		return nil // no DEFAULT is emitted for an empty value; nothing to guard
	}
	if defaultKeywords[strings.ToUpper(val)] {
		return nil
	}
	if q := val[0]; q == '\'' || q == '"' {
		end, ok := skipQuoted(val, 0, q)
		if !ok {
			return fmt.Errorf("column %s: invalid default %q: unterminated %c-quoted string", col, val, q)
		}
		if end != len(val)-1 {
			return fmt.Errorf(
				"column %s: invalid default %q: text after the closing %c. A quoted default must be one literal, e.g. \"'draft'\"",
				col, val, q)
		}
		return nil
	}
	switch sqlType {
	case "INTEGER", "REAL", "BOOLEAN":
		if !numericDefaultRe.MatchString(val) {
			return fmt.Errorf(
				"column %s: invalid default %q for a %s column: expected a number (e.g. 0, 1, -2.5) or a quoted literal",
				col, val, sqlType)
		}
	}
	return nil
}
