package db

// SQL statement splitter for migration files. Semicolons inside comments and
// string literals must not split statements, so the walker tokenises just
// enough of the input to skip line comments (`-- ... \n`), block comments
// (`/* ... */`), and single-quoted string literals (with the `''` escape)
// before splitting on `;`. Double-quoted and bracketed identifiers are
// treated as plain text.

import "strings"

// splitSQLStatements returns the input SQL split into individual statements
// on top-level semicolons. Comments are stripped from the output so each
// returned statement is the bare SQL the driver should execute. Empty
// statements (pure whitespace / comments) are filtered out.
func splitSQLStatements(sql string) []string {
	var (
		out     []string
		current strings.Builder
		i       int
		n       = len(sql)
	)
	flush := func() {
		s := strings.TrimSpace(current.String())
		if s != "" {
			out = append(out, s)
		}
		current.Reset()
	}
	for i < n {
		c := sql[i]

		// Line comment: -- ... newline
		if c == '-' && i+1 < n && sql[i+1] == '-' {
			// Skip to end of line (or end of input).
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				i = n
			} else {
				// Preserve the newline so the SQL keeps a token boundary.
				current.WriteByte('\n')
				i += j + 1
			}
			continue
		}

		// Block comment: /* ... */
		if c == '/' && i+1 < n && sql[i+1] == '*' {
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				i = n
			} else {
				// Replace the block with a single space so adjacent tokens
				// don't fuse together.
				current.WriteByte(' ')
				i += end + 4
			}
			continue
		}

		// Single-quoted string literal: copy verbatim, including the
		// surrounding quotes. Standard SQL `''` is an escaped quote inside
		// the string, not a terminator.
		if c == '\'' {
			current.WriteByte(c)
			i++
			for i < n {
				if sql[i] == '\'' {
					current.WriteByte('\'')
					i++
					if i < n && sql[i] == '\'' {
						// Escaped quote, keep scanning.
						current.WriteByte('\'')
						i++
						continue
					}
					break
				}
				current.WriteByte(sql[i])
				i++
			}
			continue
		}

		// Statement terminator at top level.
		if c == ';' {
			flush()
			i++
			continue
		}

		current.WriteByte(c)
		i++
	}
	flush()
	return out
}
