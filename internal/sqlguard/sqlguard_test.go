package sqlguard

import "testing"

func TestHasStackedStatements(t *testing.T) {
	cases := []struct {
		name   string
		q      string
		want   bool
		reason string
	}{
		// --- Not stacked ---
		{"single statement", "SELECT * FROM t", false, "no separator"},
		{"trailing semicolon", "SELECT * FROM t;", false, "a single trailing ; is tolerated"},
		{"trailing semicolon + whitespace", "SELECT * FROM t;   \n\t", false, "trailing whitespace after ; is tolerated"},
		{"trailing semicolon + line comment", "SELECT 1; -- done", false, "a trailing ; followed only by a comment is tolerated"},
		{"trailing semicolon + block comment", "SELECT 1; /* end */", false, "trailing block comment after ; is tolerated"},
		{"empty", "", false, "nothing to separate"},

		// --- Semicolon hidden inside quoted regions (must NOT count) ---
		{"semicolon in single-quoted string", "SELECT ';' AS x", false, "; inside a '...' literal is data, not a separator"},
		{"semicolon in double-quoted ident", `SELECT "a;b" FROM t`, false, "; inside a \"...\" identifier is not a separator"},
		{"semicolon in backtick ident", "SELECT `a;b` FROM t", false, "; inside a `...` identifier is not a separator"},
		{"semicolon in bracket ident", "SELECT [a;b] FROM t", false, "; inside a [...] identifier is not a separator"},
		{"escaped quote then semicolon in string", "SELECT 'it''s; fine' AS x", false, "doubled '' stays in the literal, so the ; is still data"},
		{"semicolon in line comment", "SELECT 1 -- a; b\n", false, "; inside a -- comment is ignored"},
		{"semicolon in block comment", "SELECT 1 /* a; b */ FROM t", false, "; inside a block comment is ignored"},

		// --- Genuinely stacked (MUST count) ---
		{"two statements", "SELECT 1; DROP TABLE t", true, "a real ; separates two statements"},
		{"drop after select", "SELECT * FROM t; DELETE FROM t", true, "classic stacked-injection shape"},
		{"semicolon then comment then statement", "SELECT 1; -- x\nDROP TABLE t", true, "a statement follows the comment after the ;"},
		{"statement after closed string", "SELECT ';'; DROP TABLE t", true, "the second ; (outside the literal) is a real separator"},
		{"statement after closed bracket ident", "SELECT [a;b]; DROP TABLE t", true, "the ; after the closing ] is a real separator"},
		{"three statements", "INSERT INTO t VALUES(1); UPDATE t SET x=2; SELECT 1", true, "multiple real separators"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasStackedStatements(c.q); got != c.want {
				t.Errorf("HasStackedStatements(%q) = %v, want %v (%s)", c.q, got, c.want, c.reason)
			}
		})
	}
}

func TestLeadingVerb(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want string
	}{
		{"plain select", "SELECT * FROM t", "SELECT"},
		{"lowercase upcased", "select 1", "SELECT"},
		{"leading whitespace", "   \n\t  UPDATE t SET x=1", "UPDATE"},
		{"leading line comment", "-- comment\nDELETE FROM t", "DELETE"},
		{"leading block comment", "/* comment */ INSERT INTO t VALUES(1)", "INSERT"},
		{"stacked comments and whitespace", "  -- one\n  /* two */\n  DROP TABLE t", "DROP"},
		{"comment with no newline is not a verb", "-- just a comment", ""},
		{"unterminated block comment", "/* never closes", ""},
		{"empty", "", ""},
		{"whitespace only", "   \n\t ", ""},
		// A verb must not be smuggled in behind a comment prefix: stripping the
		// leading comment must expose the real first token, not hide it.
		{"comment prefix does not hide verb", "/*x*/DROP TABLE t", "DROP"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LeadingVerb(c.q); got != c.want {
				t.Errorf("LeadingVerb(%q) = %q, want %q", c.q, got, c.want)
			}
		})
	}
}
