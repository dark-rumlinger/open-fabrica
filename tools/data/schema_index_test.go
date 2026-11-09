package data

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// TestSchemaAddIndex_MissingColumnSuggests verifies a B-tree index over a column
// the table lacks fails with the table's real columns + a did-you-mean, rather
// than SQLite's bare "no such column" (which cost the model a describe round-trip).
func TestSchemaAddIndex_MissingColumnSuggests(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}

	// Typo: 'slugg' should be caught pre-exec and point at 'slug'.
	res, err := SchemaAddIndex(ctx, "models", []any{"slugg"}, false)
	if err != nil {
		t.Fatalf("expected a Result, got go error: %v", err)
	}
	if res.Success {
		t.Fatal("indexing a non-existent column should fail")
	}
	if !strings.Contains(res.Error, "do not exist") || !strings.Contains(res.Error, "Did you mean") || !strings.Contains(res.Error, "slug") {
		t.Errorf("expected a did-you-mean pointing at slug, got: %s", res.Error)
	}
	// The real column list is surfaced so no describe_table is needed.
	if !strings.Contains(res.Error, "name") {
		t.Errorf("expected the actual column list in the error, got: %s", res.Error)
	}

	// No index should have been created on the failed attempt.
	var n int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name LIKE 'idx_models%'").Scan(&n)
	if n != 0 {
		t.Errorf("no index should exist after a rejected add_index, found %d", n)
	}
}

// TestSchemaAddIndex_RejectsQualifiedColumn verifies a table-qualified column name
// yields a clear "use the bare name" error instead of being silently dropped and
// mis-reported as "index_columns is required".
func TestSchemaAddIndex_RejectsQualifiedColumn(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := SchemaAddIndex(ctx, "models", []any{"models.slug"}, false)
	if err != nil {
		t.Fatalf("expected a Result, got go error: %v", err)
	}
	if res.Success {
		t.Fatal("a table-qualified column name should be rejected")
	}
	if !strings.Contains(res.Error, "invalid column name") || !strings.Contains(res.Error, "bare column name") {
		t.Errorf("expected a bare-column-name hint, got: %s", res.Error)
	}
	if strings.Contains(res.Error, "index_columns is required") {
		t.Errorf("must NOT report the misleading 'index_columns is required' when a column was supplied: %s", res.Error)
	}
}

// TestSchemaAddIndex_HappyPath confirms a valid multi-column index still builds.
func TestSchemaAddIndex_HappyPath(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := SchemaAddIndex(ctx, "models", []any{"slug", "name"}, true)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("expected success, got res=%v err=%v", res, err)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='uidx_models_slug_name'").Scan(&n)
	if n != 1 {
		t.Errorf("expected uidx_models_slug_name (unique) to be created, found %d", n)
	}
}

// TestCreateFTSIndex_RejectsHostileColumn verifies the FTS DDL builder refuses
// a column name that isn't a plain identifier, closing the interpolation gap.
func TestCreateFTSIndex_RejectsHostileColumn(t *testing.T) {
	db := setupDataDB(t)

	hostile := "name); DROP TABLE models;--"
	if err := CreateFTSIndex(db, "models", []string{hostile}); err == nil {
		t.Fatal("expected CreateFTSIndex to reject a hostile column name")
	}

	// The source table must survive: nothing should have executed.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM models").Scan(&count); err != nil {
		t.Fatalf("models table should still exist: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 seed rows intact, got %d", count)
	}
	// No FTS table should have been created.
	var ftsCount int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='models_fts'").Scan(&ftsCount)
	if ftsCount != 0 {
		t.Errorf("expected no FTS table on rejection, found %d", ftsCount)
	}
}

// TestCreateFTSIndex_AcceptsValidColumns confirms the happy path still builds
// the FTS5 virtual table for legitimate identifiers.
func TestCreateFTSIndex_AcceptsValidColumns(t *testing.T) {
	db := setupDataDB(t)

	if err := CreateFTSIndex(db, "models", []string{"name", "slug"}); err != nil {
		t.Fatalf("expected valid columns to succeed, got %v", err)
	}
	var ftsCount int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='models_fts'").Scan(&ftsCount)
	if ftsCount != 1 {
		t.Errorf("expected FTS table to be created, found %d", ftsCount)
	}
}

// TestSearchCreateIndex_RejectsHostileColumn verifies the tool entry point
// surfaces a clean error rather than executing injected DDL.
func TestSearchCreateIndex_RejectsHostileColumn(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := searchCreateIndex(ctx, "models", []any{"name", "evil) USING fts5(x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected create_index to reject a hostile column name")
	}
	if !strings.Contains(strings.ToLower(res.Error), "column") {
		t.Errorf("error should mention the bad column, got %q", res.Error)
	}
}
