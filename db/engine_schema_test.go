package db

import (
	"path/filepath"
	"testing"
)

// TestEngineSchemaMigration checks that a fresh project DB has the engine tables
// and columns the engine reads.
func TestEngineSchemaMigration(t *testing.T) {
	dir := t.TempDir()
	mgr := NewProjectDBManager(dir)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	defer mgr.CloseAll()

	if _, err := filepath.Abs(dir); err != nil {
		t.Fatal(err)
	}

	wantTables := []string{
		"of_runs", "of_run_log", "of_llm_calls", "of_llm_cache",
		"of_tasks", "of_sessions", "of_runtime_errors",
	}
	for _, table := range wantTables {
		var n int
		if err := pdb.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&n); err != nil {
			t.Fatalf("checking table %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}

	// of_tasks has the run_id column.
	var n int
	if err := pdb.QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info('of_tasks') WHERE name='run_id'",
	).Scan(&n); err != nil {
		t.Fatalf("checking of_tasks.run_id: %v", err)
	}
	if n != 1 {
		t.Error("of_tasks.run_id column missing")
	}

	// of_ws_endpoints carries the join-seed columns.
	for _, col := range []string{"seed_table", "seed_key_column", "seed_columns"} {
		var c int
		if err := pdb.QueryRow(
			"SELECT COUNT(*) FROM pragma_table_info('of_ws_endpoints') WHERE name=?", col,
		).Scan(&c); err != nil {
			t.Fatalf("checking of_ws_endpoints.%s: %v", col, err)
		}
		if c != 1 {
			t.Errorf("of_ws_endpoints.%s column missing", col)
		}
	}
}
