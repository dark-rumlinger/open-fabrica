package data

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// TestNormalizeCheckExpr covers both check shapes the DDL builder must accept:
// bare operator fragments (column reference implied, prepend the name) and full
// predicates that already reference the column (emit verbatim). The full-predicate
// case must not be double-prefixed into "CHECK (col col IN (...))", a SQLite
// syntax error.
func TestNormalizeCheckExpr(t *testing.T) {
	cases := []struct {
		name  string
		col   string
		check string
		want  string
	}{
		{"bare comparison", "qty", "> 0", "qty > 0"},
		{"bare gte", "age", ">= 18", "age >= 18"},
		{"bare in", "status", "IN ('a','b')", "status IN ('a','b')"},
		{"bare in no space", "status", "IN('a','b')", "status IN('a','b')"},
		{"bare not in", "kind", "NOT IN ('x')", "kind NOT IN ('x')"},
		{"bare like", "code", "LIKE 'A%'", "code LIKE 'A%'"},
		{"full predicate in", "discipline", "discipline IN ('Branding','Editorial','Web','Type')", "discipline IN ('Branding','Editorial','Web','Type')"},
		{"full predicate func", "name", "length(name) > 0", "length(name) > 0"},
		{"empty falls back to column", "flag", "   ", "flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeCheckExpr(tc.col, tc.check); got != tc.want {
				t.Errorf("normalizeCheckExpr(%q, %q) = %q, want %q", tc.col, tc.check, got, tc.want)
			}
		})
	}
}

// TestSchemaCreate_FullPredicateCheck is the end-to-end regression for the
// reported failure: a column whose check already names the column must create
// the table cleanly and actually enforce the constraint, rather than failing
// with `near "discipline": syntax error`.
func TestSchemaCreate_FullPredicateCheck(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	cols := map[string]any{
		"title": map[string]any{"type": "TEXT", "not_null": true},
		"discipline": map[string]any{
			"type":     "TEXT",
			"not_null": true,
			"check":    "discipline IN ('Branding','Editorial','Web','Type')",
		},
	}

	res, err := SchemaCreate(ctx, "projects", cols, nil)
	if err != nil {
		t.Fatalf("SchemaCreate returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("SchemaCreate failed: %s", res.Error)
	}

	// Physical name equals the table name for user tables.
	// Valid value is accepted.
	if _, err := db.Exec("INSERT INTO projects (title, discipline) VALUES ('A', 'Web')"); err != nil {
		t.Fatalf("valid discipline insert should succeed: %v", err)
	}
	// Invalid value is rejected by the CHECK constraint, proving it was applied.
	if _, err := db.Exec("INSERT INTO projects (title, discipline) VALUES ('B', 'Painting')"); err == nil {
		t.Fatal("invalid discipline should be rejected by the CHECK constraint")
	} else if !strings.Contains(strings.ToUpper(err.Error()), "CHECK") {
		t.Errorf("expected a CHECK constraint failure, got: %v", err)
	}
}
