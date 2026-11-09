package api

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// TestAPICreate_NonUniqueLookupWarns guards the create-time nudge for a
// non-unique lookup column: an endpoint whose lookup_column is a non-unique
// column can't address (or upsert) a single row by /api/<path>/<value>. A unique
// lookup_column must NOT warn.
func TestAPICreate_NonUniqueLookupWarns(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	mustExec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	// A non-unique lookup: id PK, a non-unique position column used as lookup.
	mustExec(`CREATE TABLE world_edits (id INTEGER PRIMARY KEY AUTOINCREMENT, pos TEXT NOT NULL, block_type TEXT NOT NULL)`)
	mustExec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('world_edits','[]')`)
	// A table whose lookup column IS unique.
	mustExec(`CREATE TABLE blocks (id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT NOT NULL UNIQUE, label TEXT)`)
	mustExec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('blocks','[]')`)

	// Non-unique lookup_column -> warning naming the column.
	res, err := (&APICreate{
		EndpointPath: "world-edits", TableName: "world_edits",
		Methods: []string{"GET", "PUT"}, LookupColumn: "pos",
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create world-edits: err=%v res=%+v", err, res)
	}
	data, _ := res.Data.(map[string]any)
	warn, _ := data["key_warning"].(string)
	if warn == "" || !strings.Contains(warn, "pos") {
		t.Errorf("expected key_warning naming non-unique lookup_column 'pos'; got %q (data=%+v)", warn, data)
	}

	// Unique lookup_column -> no warning.
	res2, err := (&APICreate{
		EndpointPath: "blocks", TableName: "blocks",
		Methods: []string{"GET", "PUT"}, LookupColumn: "code",
	}).Run(ctx)
	if err != nil || res2 == nil || !res2.Success {
		t.Fatalf("create blocks: err=%v res=%+v", err, res2)
	}
	data2, _ := res2.Data.(map[string]any)
	if w, ok := data2["key_warning"]; ok {
		t.Errorf("unique lookup_column must not warn; got key_warning=%v", w)
	}
}
