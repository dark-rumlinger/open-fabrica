package data

import (
	"testing"

	"openfabrica/tools"
)

// validateReadOnlySELECT is the shared guard for raw SQL the LLM supplies
// (execute_sql runs it, create_view embeds it). These cases pin the contract:
// SELECT-only, single statement, and no reference to any reserved of_ table -
// while ordinary columns that merely contain "of_" mid-identifier still pass.
func TestValidateReadOnlySELECT(t *testing.T) {
	tests := []struct {
		name      string
		sql       string
		wantBlock bool
	}{
		{"plain select", "SELECT id, title FROM posts", false},
		{"non-select rejected", "UPDATE posts SET title = 'x'", true},
		{"multi-statement rejected", "SELECT 1; DROP TABLE posts", true},
		{"system table at FROM", "SELECT * FROM of_secrets", true},
		{"system table lowercased match", "select value_encrypted from OF_SECRETS", true},
		{"system table via join", "SELECT p.id FROM posts p JOIN of_users u ON u.id = p.user_id", true},
		{"system table newer of_ table", "SELECT * FROM of_some_future_table", true},
		// of_ mid-identifier is a real user column, not a system-table reference.
		{"user column contains of_", "SELECT number_of_items FROM sales", false},
		{"user column ends in of", "SELECT cost_of_goods FROM ledger", false},
		// A read-only CTE (WITH ... SELECT) is a SELECT and must pass; a WITH-wrapped
		// write must still be blocked, and a CTE touching a system table still caught.
		{"recursive cte", "WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<30) SELECT i FROM seq", false},
		{"cte select", "WITH t AS (SELECT id FROM posts) SELECT COUNT(*) FROM t", false},
		{"cte wrapping a write", "WITH t AS (SELECT id FROM posts) DELETE FROM posts WHERE id IN (SELECT id FROM t)", true},
		{"cte over system table", "WITH s AS (SELECT * FROM of_secrets) SELECT * FROM s", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, rejectErr := validateReadOnlySELECT(tc.sql)
			if tc.wantBlock && rejectErr == "" {
				t.Errorf("expected %q to be blocked, but it passed", tc.sql)
			}
			if !tc.wantBlock && rejectErr != "" {
				t.Errorf("expected %q to pass, blocked with: %s", tc.sql, rejectErr)
			}
		})
	}
}

// The row cap must hold even when the query carries its own larger LIMIT, mentions
// the word elsewhere, or ends in a line comment.
func TestDataExecuteSQLCapsRows(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	cases := []string{
		"SELECT * FROM models LIMIT 1000",
		"SELECT slug AS limit_label FROM models",
		"SELECT * FROM models -- all of them",
	}
	for _, q := range cases {
		res, err := DataExecuteSQL(ctx, &SQLExecute{SQL: q, Limit: 2})
		if err != nil || !res.Success {
			t.Fatalf("%q: err=%v result=%+v", q, err, res)
		}
		if got := res.Data.(map[string]any)["count"]; got != 2 {
			t.Errorf("%q: count = %v, want 2 (the limit arg must cap it)", q, got)
		}
	}
}
