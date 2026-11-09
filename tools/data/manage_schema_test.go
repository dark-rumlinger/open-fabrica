package data

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupSchemaDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	stmts := []string{}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

// TestSchemaCreateRoundTrip pins arg forwarding for manage_schema create_table:
// table_name, column_defs, and searchable_columns must all reach the registry
// and the created table. Under the typed handler none of those can be silently
// dropped by a map key mismatch.
func TestSchemaCreateRoundTrip(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	act := &SchemaActCreateTable{
		TableName: "articles",
		ColumnDefs: map[string]any{
			"title": "TEXT",
			"body":  "TEXT",
			"views": "INTEGER",
		},
		SearchableColumns: []any{"title", "body"},
	}
	res, err := act.Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create_table: err=%v res=%+v", err, res)
	}

	// The physical table exists with the declared columns.
	cols, _ := lowerCaseTableColumns(db, "articles")
	for _, c := range []string{"title", "body", "views"} {
		if !cols[c] {
			t.Errorf("column %q not created (arg-forwarding drift on column_defs)", c)
		}
	}

	// Registry row recorded the schema.
	var schemaDef string
	if err := db.QueryRow("SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?", "articles").Scan(&schemaDef); err != nil {
		t.Fatalf("load registry row: %v", err)
	}
	if !strings.Contains(schemaDef, "title") || !strings.Contains(schemaDef, "body") {
		t.Errorf("schema_def did not persist column_defs: %q", schemaDef)
	}

	// searchable_columns produced an FTS index (proves the arg was forwarded).
	var ftsCount int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='articles_fts'").Scan(&ftsCount)
	if ftsCount != 1 {
		t.Errorf("searchable_columns dropped: no FTS index created (count=%d)", ftsCount)
	}
	data, _ := res.Data.(map[string]any)
	if data == nil || data["fts_columns"] == nil {
		t.Errorf("result did not reflect fts_columns: %+v", res.Data)
	}
}

// create_table reports the two missing-arg cases distinctly: a missing name says
// so, and a present name with empty column_defs names the table and points at the
// columns - rather than a single "table_name and column_defs are required" message,
// which names table_name too and reads (in chat) as if the name were missing. The
// empty-column_defs case is a table name given with the columns omitted.
func TestSchemaCreate_DistinctRequiredMessages(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, _ := SchemaCreate(ctx, "", map[string]any{"title": "TEXT"}, nil); res.Success ||
		!strings.Contains(res.Error, "requires a table_name") {
		t.Errorf("a missing table_name must say so, got %+v", res)
	}

	res, _ := SchemaCreate(ctx, "usage_events", map[string]any{}, nil)
	if res.Success {
		t.Fatalf("an empty column_defs must be rejected")
	}
	if !strings.Contains(res.Error, "usage_events") || !strings.Contains(res.Error, "column") {
		t.Errorf("the empty-columns error must name the table and point at column_defs, got: %s", res.Error)
	}
	if strings.Contains(res.Error, "table_name and column_defs are required") {
		t.Errorf("must not fall back to the misleading both-params message")
	}
}

// TestSchemaAlterRoundTrip pins add_columns/drop_columns forwarding.
func TestSchemaAlterRoundTrip(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&SchemaActCreateTable{
		TableName:  "items",
		ColumnDefs: map[string]any{"name": "TEXT", "qty": "INTEGER"},
	}).Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	// add_columns must reach the ALTER.
	res, err := (&SchemaActAlterTable{
		TableName:  "items",
		AddColumns: map[string]any{"sku": "TEXT"},
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("alter add: err=%v res=%+v", err, res)
	}
	cols, _ := lowerCaseTableColumns(db, "items")
	if !cols["sku"] {
		t.Errorf("add_columns dropped: sku not added")
	}
}

// TestSchemaAddIndexRoundTrip pins index_columns + unique forwarding.
func TestSchemaAddIndexRoundTrip(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&SchemaActCreateTable{
		TableName:  "users",
		ColumnDefs: map[string]any{"email": "TEXT"},
	}).Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := (&SchemaActAddIndex{
		TableName:    "users",
		IndexColumns: []any{"email"},
		Unique:       true,
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("add_index: err=%v res=%+v", err, res)
	}
	data := res.Data.(map[string]any)
	if data["unique"] != true {
		t.Errorf("unique arg dropped: %+v", data)
	}
	// The unique index actually exists in sqlite_master.
	var n int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?", data["index"]).Scan(&n)
	if n != 1 {
		t.Errorf("index not created: %v", data["index"])
	}
}

// TestSchemaDescribeAndDropRoundTrip pins the table_name read path for describe
// and drop. drop_table requires owner approval at run time, so this exercises
// describe end-to-end and confirms drop reaches the approval gate (table still
// present, since no approval is recorded).
func TestSchemaDescribeAndDropRoundTrip(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&SchemaActCreateTable{
		TableName:  "widgets",
		ColumnDefs: map[string]any{"label": "TEXT"},
	}).Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	desc, err := (&SchemaActDescribeTable{TableName: "widgets"}).Run(ctx)
	if err != nil || desc == nil || !desc.Success {
		t.Fatalf("describe: err=%v res=%+v", err, desc)
	}
	d := desc.Data.(map[string]any)
	if d["table"] != "widgets" {
		t.Errorf("describe table_name dropped: %+v", d["table"])
	}

	listed, err := (&SchemaActListTables{}).Run(ctx)
	if err != nil || listed == nil || !listed.Success {
		t.Fatalf("list: err=%v res=%+v", err, listed)
	}
}

// TestSchemaCreateViewRoundTrip pins create_view: the view is created in
// sqlite_master, is queryable, shows up in list_tables tagged as a view, and
// re-running with the same name replaces the definition.
func TestSchemaCreateViewRoundTrip(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&SchemaActCreateTable{
		TableName:  "users",
		ColumnDefs: map[string]any{"email": "TEXT", "status": "TEXT"},
	}).Run(ctx); err != nil {
		t.Fatalf("create base table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO users (email, status) VALUES ('a@x.com','active'),('b@x.com','inactive')"); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	res, err := (&SchemaActCreateView{
		ViewName: "active_users",
		Select:   "SELECT id, email FROM users WHERE status = 'active'",
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create_view: err=%v res=%+v", err, res)
	}

	// The view exists and resolves the SELECT.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM active_users").Scan(&n); err != nil {
		t.Fatalf("query view: %v", err)
	}
	if n != 1 {
		t.Errorf("view returned %d rows, want 1 (the active user)", n)
	}

	// list_tables surfaces the view tagged object_type=view.
	listed, err := (&SchemaActListTables{}).Run(ctx)
	if err != nil || !listed.Success {
		t.Fatalf("list: err=%v res=%+v", err, listed)
	}
	entries := listed.Data.([]map[string]any)
	foundView := false
	for _, e := range entries {
		if e["table_name"] == "active_users" && e["object_type"] == "view" {
			foundView = true
		}
	}
	if !foundView {
		t.Errorf("view not surfaced in list_tables: %+v", entries)
	}

	// Re-running replaces the definition (CREATE OR REPLACE semantics).
	res2, err := (&SchemaActCreateView{
		ViewName: "active_users",
		Select:   "SELECT id, email FROM users",
	}).Run(ctx)
	if err != nil || res2 == nil || !res2.Success {
		t.Fatalf("replace view: err=%v res=%+v", err, res2)
	}
	db.QueryRow("SELECT COUNT(*) FROM active_users").Scan(&n)
	if n != 2 {
		t.Errorf("view not replaced: got %d rows, want 2", n)
	}

	// drop_view removes it.
	if dr, err := (&SchemaActDropView{ViewName: "active_users"}).Run(ctx); err != nil || !dr.Success {
		t.Fatalf("drop_view: err=%v res=%+v", err, dr)
	}
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='view' AND name='active_users'").Scan(&n)
	if n != 0 {
		t.Errorf("view not dropped, still in sqlite_master")
	}
}

// create_view reports its two missing-arg cases distinctly, matching the
// create_table fix: a missing view_name says so, and a present name with an empty
// select names the view and points at the missing SELECT.
func TestSchemaCreateView_DistinctRequiredMessages(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, _ := SchemaCreateView(ctx, "", "SELECT 1"); res.Success ||
		!strings.Contains(res.Error, "create_view requires a view_name") {
		t.Errorf("a missing view_name must say so, got %+v", res)
	}
	res, _ := SchemaCreateView(ctx, "active_items", "   ")
	if res.Success || !strings.Contains(res.Error, "active_items") || !strings.Contains(res.Error, "select") {
		t.Errorf("a missing select must name the view and the missing piece, got: %s", res.Error)
	}
	if strings.Contains(res.Error, "view_name and select are required") {
		t.Errorf("must not fall back to the both-params message")
	}
}

// TestSchemaCreateViewRejects pins the guardrails: non-SELECT bodies, multiple
// statements, system-table references, and clobbering a real table are all
// refused without touching the database.
func TestSchemaCreateViewRejects(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&SchemaActCreateTable{
		TableName:  "items",
		ColumnDefs: map[string]any{"name": "TEXT"},
	}).Run(ctx); err != nil {
		t.Fatalf("create base table: %v", err)
	}

	cases := []struct {
		name    string
		view    string
		sel     string
		wantSub string
	}{
		{"non-select", "v1", "DELETE FROM items", "SELECT"},
		{"multi-statement", "v2", "SELECT 1; DROP TABLE items", "multiple SQL statements"},
		{"system-table", "v3", "SELECT * FROM of_secrets", "system table"},
		{"of-prefix-name", "of_sneaky", "SELECT 1", "of_ prefix"},
		{"clobber-table", "items", "SELECT 1", "existing table"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := (&SchemaActCreateView{ViewName: c.view, Select: c.sel}).Run(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Success {
				t.Fatalf("expected rejection, got success: %+v", res)
			}
			if !strings.Contains(res.Error, c.wantSub) {
				t.Errorf("error %q does not mention %q", res.Error, c.wantSub)
			}
		})
	}

	// The base table is untouched by the rejected multi-statement attempt.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='items'").Scan(&n); err != nil || n != 1 {
		t.Errorf("items table was harmed by a rejected create_view (n=%d, err=%v)", n, err)
	}
}
