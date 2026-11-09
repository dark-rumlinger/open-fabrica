package db

import (
	"strings"
	"testing"
)

// TestSplitSQLStatementsBasic locks in the happy path: two statements
// separated by a semicolon.
func TestSplitSQLStatementsBasic(t *testing.T) {
	in := "CREATE TABLE a (id INTEGER); CREATE TABLE b (id INTEGER);"
	got := splitSQLStatements(in)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE a") || !strings.HasPrefix(got[1], "CREATE TABLE b") {
		t.Errorf("statements not split correctly: %q", got)
	}
}

// TestSplitSQLStatementsCommentWithSemicolon guards the splitter against a line
// comment containing a `;`, which must not break the statement around it.
func TestSplitSQLStatementsCommentWithSemicolon(t *testing.T) {
	in := `-- when the worker picks up the build; status is written here
CREATE TABLE of_runs (id TEXT);`
	got := splitSQLStatements(in)
	if len(got) != 1 {
		t.Fatalf("got %d statements, want 1: %q", len(got), got)
	}
	if !strings.Contains(got[0], "CREATE TABLE of_runs") {
		t.Errorf("statement lost its CREATE TABLE body: %q", got[0])
	}
	if strings.Contains(got[0], "--") || strings.Contains(got[0], "status is written") {
		t.Errorf("comment leaked into output: %q", got[0])
	}
}

// TestSplitSQLStatementsBlockComment ensures block comments are stripped and
// any semicolons inside them ignored.
func TestSplitSQLStatementsBlockComment(t *testing.T) {
	in := `/* note: drop; recreate next */ CREATE TABLE a (id INTEGER);`
	got := splitSQLStatements(in)
	if len(got) != 1 {
		t.Fatalf("got %d statements, want 1: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE a") {
		t.Errorf("block comment not stripped cleanly: %q", got[0])
	}
}

// TestSplitSQLStatementsStringWithSemicolon ensures semicolons inside
// single-quoted strings stay inside the statement.
func TestSplitSQLStatementsStringWithSemicolon(t *testing.T) {
	in := `INSERT INTO settings (key, value) VALUES ('motd', 'hello; world'); SELECT 1;`
	got := splitSQLStatements(in)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2: %q", len(got), got)
	}
	if !strings.Contains(got[0], "'hello; world'") {
		t.Errorf("string contents fractured: %q", got[0])
	}
}

// TestSplitSQLStatementsEscapedQuote covers the standard SQL ”-escape inside
// a string literal.
func TestSplitSQLStatementsEscapedQuote(t *testing.T) {
	in := `INSERT INTO t VALUES ('it''s; fine'); SELECT 2;`
	got := splitSQLStatements(in)
	if len(got) != 2 {
		t.Fatalf("got %d statements, want 2: %q", len(got), got)
	}
	if !strings.Contains(got[0], "'it''s; fine'") {
		t.Errorf("escaped-quote string fractured: %q", got[0])
	}
}

// TestSplitSQLStatementsCommentInsideTable mirrors the real migration shape:
// a CREATE TABLE with `-- comment` lines between columns. None of the
// interior comments should split the statement.
func TestSplitSQLStatementsCommentInsideTable(t *testing.T) {
	in := `CREATE TABLE of_runs (
    id TEXT PRIMARY KEY,
    -- run status: running; then completed once done.
    status TEXT
);`
	got := splitSQLStatements(in)
	if len(got) != 1 {
		t.Fatalf("interior comment split the statement into %d pieces: %q", len(got), got)
	}
}

// TestSplitSQLStatementsEmptyAndWhitespace ensures blank input doesn't yield
// spurious empty statements.
func TestSplitSQLStatementsEmptyAndWhitespace(t *testing.T) {
	cases := []string{"", "   ", "-- just a comment\n", ";", "  ;  ;  "}
	for _, in := range cases {
		got := splitSQLStatements(in)
		if len(got) != 0 {
			t.Errorf("input %q produced %d statements, want 0: %q", in, len(got), got)
		}
	}
}
