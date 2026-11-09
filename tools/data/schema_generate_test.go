package data

import (
	"database/sql"
	"strings"
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

func TestGeneratorDefaultSQL(t *testing.T) {
	cases := map[string]string{
		"code":                "(lower(hex(randomblob(4))))",
		"token":               "(lower(hex(randomblob(16))))",
		"expires_in_days:7":   "(datetime('now','+7 days'))",
		"expires_in_hours:12": "(datetime('now','+12 hours'))",
	}
	for kind, want := range cases {
		got, err := generatorDefaultSQL(kind)
		if err != nil {
			t.Errorf("generatorDefaultSQL(%q) errored: %v", kind, err)
			continue
		}
		if got != want {
			t.Errorf("generatorDefaultSQL(%q) = %q, want %q", kind, got, want)
		}
	}
	// uuid is dashed 8-4-4-4-12.
	if got, _ := generatorDefaultSQL("uuid"); !strings.Contains(got, "'-'") || !strings.Contains(got, "randomblob(6)") {
		t.Errorf("uuid expr shape unexpected: %q", got)
	}
	for _, bad := range []string{"expires_in_days:0", "expires_in_days:-3", "expires_in_days:abc", "bogus", ""} {
		if _, err := generatorDefaultSQL(bad); err == nil {
			t.Errorf("generatorDefaultSQL(%q) should have errored", bad)
		}
	}
}

func TestParseColumnDef_Generate(t *testing.T) {
	cd, err := parseColumnDef("slug", map[string]any{"type": "TEXT", "unique": true, "generate": "code"})
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if cd.DefaultExpr != "(lower(hex(randomblob(4))))" {
		t.Errorf("DefaultExpr = %q", cd.DefaultExpr)
	}
	if got := cd.columnSQL(); !strings.Contains(got, "DEFAULT (lower(hex(randomblob(4))))") {
		t.Errorf("columnSQL should emit the expression verbatim (unquoted), got: %s", got)
	}
	// A generated column must not be double-declared with a literal default.
	if _, err := parseColumnDef("x", map[string]any{"type": "TEXT", "default": "a", "generate": "code"}); err == nil {
		t.Error("expected error when both default and generate are set")
	}
	// Generated values are text; a non-TEXT column is rejected.
	if _, err := parseColumnDef("x", map[string]any{"type": "INTEGER", "generate": "code"}); err == nil {
		t.Error("expected error for a non-TEXT generated column")
	}
}

// TestGeneratedColumnFillsOnOmittedInsert runs the real CREATE TABLE SQL that
// columnSQL builds, then inserts WITHOUT the generated columns - exactly the way
// an upload/CRUD auto-insert omits them. SQLite must fill them from the DEFAULT
// expression, satisfying NOT NULL + UNIQUE, so the app needs no server function.
func TestGeneratedColumnFillsOnOmittedInsert(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	code, _ := parseColumnDef("code", map[string]any{"type": "TEXT", "not_null": true, "unique": true, "generate": "code"})
	exp, _ := parseColumnDef("expires_at", map[string]any{"type": "TEXT", "not_null": true, "generate": "expires_in_days:7"})
	fn, _ := parseColumnDef("filename", map[string]any{"type": "TEXT", "not_null": true})
	create := "CREATE TABLE files (id INTEGER PRIMARY KEY AUTOINCREMENT, " +
		code.columnSQL() + ", " + exp.columnSQL() + ", " + fn.columnSQL() + ")"
	if _, err := db.Exec(create); err != nil {
		t.Fatalf("create table (%s): %v", create, err)
	}

	for i := 0; i < 2; i++ {
		if _, err := db.Exec("INSERT INTO files (filename) VALUES (?)", "a.jpg"); err != nil {
			t.Fatalf("omitted-column insert %d failed: %v", i, err)
		}
	}

	rows, err := db.Query("SELECT code, expires_at FROM files ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var codes []string
	for rows.Next() {
		var c, e string
		if err := rows.Scan(&c, &e); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(c) != 8 {
			t.Errorf("generated code = %q, want 8 hex chars", c)
		}
		if !strings.Contains(e, "-") {
			t.Errorf("expires_at = %q, want a datetime string", e)
		}
		codes = append(codes, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rows: %v", err)
	}
	if len(codes) != 2 || codes[0] == codes[1] {
		t.Errorf("expected two DISTINCT generated codes, got %v", codes)
	}
}

// TestSchemaCreate_GeneratedColumnEndToEnd exercises the whole create_table
// handler: a table with a generated `code` and `expires_at` is created, and an
// insert that omits both (as an upload/CRUD auto-insert would) gets them filled.
func TestSchemaCreate_GeneratedColumnEndToEnd(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := SchemaCreate(ctx, "files", map[string]any{
		"code":       map[string]any{"type": "TEXT", "not_null": true, "unique": true, "generate": "code"},
		"expires_at": map[string]any{"type": "TEXT", "not_null": true, "generate": "expires_in_days:7"},
		"filename":   map[string]any{"type": "TEXT", "not_null": true},
	}, nil)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create_table with generated columns failed: err=%v res=%+v", err, res)
	}
	if _, err := db.Exec("INSERT INTO files (filename) VALUES ('a.jpg')"); err != nil {
		t.Fatalf("omitted-column insert failed: %v", err)
	}
	var code, exp string
	if err := db.QueryRow("SELECT code, expires_at FROM files").Scan(&code, &exp); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(code) != 8 || exp == "" {
		t.Fatalf("generated values not filled: code=%q expires_at=%q", code, exp)
	}
}

// TestSchemaAlter_RejectsGeneratedColumn pins our guard: a generated column can
// only be declared at create_table time. Adding one via alter_table add_columns
// is rejected with guidance (SQLite's expression-default-on-ADD-COLUMN behavior
// is non-portable/ambiguous for existing rows).
func TestSchemaAlter_RejectsGeneratedColumn(t *testing.T) {
	db := setupSchemaDB(t)
	ctx := &tools.ToolContext{DB: db}
	if res, err := SchemaCreate(ctx, "widgets", map[string]any{"name": "TEXT"}, nil); err != nil || !res.Success {
		t.Fatalf("seed create: err=%v res=%+v", err, res)
	}
	res, err := SchemaAlter(ctx, "widgets", map[string]any{
		"code": map[string]any{"type": "TEXT", "generate": "code"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected alter_table to reject a generated column")
	}
	if !strings.Contains(res.Error, "create_table") {
		t.Errorf("rejection should point to create_table; got %q", res.Error)
	}
}
