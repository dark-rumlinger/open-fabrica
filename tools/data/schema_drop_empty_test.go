package data

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupDropDB builds the registry, question, and run tables the approval gate
// touches, so SchemaDrop / SchemaAlter can run their full path in-memory.
func setupDropDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)

	for _, stmt := range []string{} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup: %v\n%s", err, stmt)
		}
	}
	return db
}

// mustRegisterTable creates a physical table and its registry row, mirroring
// what create_table leaves behind.
func mustRegisterTable(t *testing.T, db dbh.Handle, name, ddl string) {
	t.Helper()
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES (?, '{}')`, name); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

// TestSchemaDrop_EmptyTableSkipsApproval: the approval gate protects data, and
// an empty table has none, so dropping one must not pause the run for an owner
// decision. Recreating is the only way to correct a table's column set.
func TestSchemaDrop_EmptyTableSkipsApproval(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustRegisterTable(t, db, "dj_tracks",
		`CREATE TABLE dj_tracks (id INTEGER PRIMARY KEY AUTOINCREMENT, dj_name TEXT NOT NULL)`)

	res, err := SchemaDrop(ctx, "dj_tracks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Code == tools.CodeApprovalRequired {
		t.Fatalf("empty table must not require approval, got %q", res.Error)
	}
	if !res.Success {
		t.Fatalf("expected drop to succeed, got %+v", res)
	}

	var still int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = 'dj_tracks'").Scan(&still)
	if still != 0 {
		t.Error("expected registry row removed")
	}
	var paused int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_questions WHERE status = 'pending'").Scan(&paused)
	if paused != 0 {
		t.Errorf("expected no approval question, got %d", paused)
	}
}

// TestSchemaDrop_ReleasesEndpointSpineRows: dropping a table must release the
// of_endpoints spine rows for its api/auth endpoints too, not just the config
// rows. A leaked spine row leaves an orphan route that answers 405/500.
func TestSchemaDrop_ReleasesEndpointSpineRows(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustRegisterTable(t, db, "posts",
		`CREATE TABLE posts (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT)`)

	// A CRUD endpoint over the table, with its spine rows, plus an unrelated
	// endpoint that must survive.
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name) VALUES ('posts', 'posts')`); err != nil {
		t.Fatalf("seed api endpoint: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES ('posts', '', 'GET', 'crud'), ('posts', '', 'POST', 'crud'), ('other', '', 'GET', 'crud')`); err != nil {
		t.Fatalf("seed spine: %v", err)
	}

	if _, err := SchemaDrop(ctx, "posts"); err != nil {
		t.Fatalf("drop: %v", err)
	}

	var postsSpine int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_endpoints WHERE path = 'posts'").Scan(&postsSpine)
	if postsSpine != 0 {
		t.Errorf("expected spine rows for dropped table released, %d remain", postsSpine)
	}
	var otherSpine int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_endpoints WHERE path = 'other'").Scan(&otherSpine)
	if otherSpine != 1 {
		t.Errorf("unrelated endpoint spine row must survive, got %d", otherSpine)
	}
}

// TestSchemaDrop_TableWithRowsStillRequiresApproval guards the exemption above
// from widening: real data still gates.
func TestSchemaDrop_TableWithRowsStillRequiresApproval(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustRegisterTable(t, db, "posts",
		`CREATE TABLE posts (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT)`)
	if _, err := db.Exec(`INSERT INTO posts (title) VALUES ('keep me')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := SchemaDrop(ctx, "posts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Code != tools.CodeApprovalRequired {
		t.Fatalf("table with rows must require approval, got %+v", res)
	}
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM posts").Scan(&n)
	if n != 1 {
		t.Errorf("row must survive an unapproved drop, got %d", n)
	}
}

// TestSchemaDrop_UncountableTableStillRequiresApproval: the row count is
// best-effort. When it cannot be read, the table is not known to be empty, so
// the gate must hold rather than read a failed count as "nothing to lose".
func TestSchemaDrop_UncountableTableStillRequiresApproval(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	// Registry row without a physical table: SELECT COUNT(*) errors.
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('phantom', '{}')`); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := SchemaDrop(ctx, "phantom")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Code != tools.CodeApprovalRequired {
		t.Fatalf("uncountable table must require approval, got %+v", res)
	}
}

// TestSchemaAlter_DropColumnsOnEmptyTableSkipsApproval: the same waiver for
// drop_columns. Columns on a table that holds no rows carry no values, so
// dropping them loses nothing.
func TestSchemaAlter_DropColumnsOnEmptyTableSkipsApproval(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustRegisterTable(t, db, "dj_tracks",
		`CREATE TABLE dj_tracks (id INTEGER PRIMARY KEY AUTOINCREMENT, dj_name TEXT NOT NULL, note TEXT)`)

	res, err := SchemaAlter(ctx, "dj_tracks", nil, []any{"dj_name"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Code == tools.CodeApprovalRequired {
		t.Fatalf("dropping a column from an empty table must not require approval, got %q", res.Error)
	}
	if !res.Success {
		t.Fatalf("expected alter to succeed, got %+v", res)
	}
	cols, _ := lowerCaseTableColumns(db, "dj_tracks")
	if cols["dj_name"] {
		t.Error("expected dj_name dropped")
	}
}

// TestSchemaAlter_DropColumnsWithRowsStillRequiresApproval guards that waiver.
func TestSchemaAlter_DropColumnsWithRowsStillRequiresApproval(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustRegisterTable(t, db, "posts",
		`CREATE TABLE posts (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT, body TEXT)`)
	if _, err := db.Exec(`INSERT INTO posts (title, body) VALUES ('t', 'b')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := SchemaAlter(ctx, "posts", nil, []any{"body"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Code != tools.CodeApprovalRequired {
		t.Fatalf("dropping a column with rows present must require approval, got %+v", res)
	}
	cols, _ := lowerCaseTableColumns(db, "posts")
	if !cols["body"] {
		t.Error("column must survive an unapproved drop")
	}
}

// TestSchemaDrop_ApprovalQuestionNamesRowCount keeps the owner-facing prompt
// honest because the count decides the gate as well as the wording.
func TestSchemaDrop_ApprovalQuestionNamesRowCount(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustRegisterTable(t, db, "posts",
		`CREATE TABLE posts (id INTEGER PRIMARY KEY AUTOINCREMENT, title TEXT)`)
	for i := 0; i < 3; i++ {
		if _, err := db.Exec(`INSERT INTO posts (title) VALUES ('x')`); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := SchemaDrop(ctx, "posts"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var q string
	if err := db.QueryRow("SELECT question FROM of_questions WHERE status = 'pending'").Scan(&q); err != nil {
		t.Fatalf("load question: %v", err)
	}
	if !strings.Contains(q, "3 rows") {
		t.Errorf("expected the prompt to name 3 rows, got %q", q)
	}
}

// A name in both add_columns and drop_columns used to be added (skipped as already
// present) and then dropped, reporting success while the column vanished. It is now
// refused with the real remedy, and the column survives.
func TestSchemaAlter_SameColumnInAddAndDropIsRefused(t *testing.T) {
	db := setupDropDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustRegisterTable(t, db, "dj_tracks",
		`CREATE TABLE dj_tracks (id INTEGER PRIMARY KEY AUTOINCREMENT, qty TEXT)`)

	res, err := SchemaAlter(ctx, "dj_tracks", map[string]any{"qty": "INTEGER"}, []any{"qty"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "NEW name") {
		t.Fatalf("expected a refusal naming the remedy, got %+v", res)
	}
	if cols, _ := lowerCaseTableColumns(db, "dj_tracks"); !cols["qty"] {
		t.Error("the refused call must leave the column in place")
	}
}
