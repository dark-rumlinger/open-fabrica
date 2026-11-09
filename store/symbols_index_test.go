package store

// Symbol-indexing integration: ArtifactSave/Delete keeping of_symbols in step
// with the artifact body. The pure extraction these assert against lives in
// analysis; this is the storage half, so it lives beside the storage.

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupSymbolDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE of_files (
			id INTEGER PRIMARY KEY,
			kind TEXT, path TEXT, body TEXT, scope TEXT,
			content_type TEXT, storage TEXT, disk_path TEXT, size INTEGER,
			metadata TEXT DEFAULT '{}', status TEXT,
			is_deleted INTEGER DEFAULT 0, deleted_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME,
			UNIQUE(kind, path)
		)`,
		`CREATE TABLE of_file_history (
			id INTEGER PRIMARY KEY,
			file_id INTEGER, kind TEXT, path TEXT,
			version_number INTEGER, changed_by TEXT,
			body_hash TEXT NOT NULL DEFAULT '',
			created_at TEXT DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_snapshot_blobs (
			hash TEXT PRIMARY KEY, body BLOB NOT NULL, size INTEGER NOT NULL,
			refcount INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_symbols (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT, kind TEXT, file_kind TEXT, file_path TEXT,
			line INTEGER DEFAULT 0, signature TEXT DEFAULT '',
			exported INTEGER DEFAULT 0, fingerprint TEXT DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_symbol_refs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			symbol_name TEXT, ref_file_kind TEXT, ref_file_path TEXT,
			ref_line INTEGER DEFAULT 0, ref_kind TEXT DEFAULT '',
			target_path TEXT DEFAULT '', fingerprint TEXT DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE of_symbol_index_state (
			file_kind TEXT, file_path TEXT, fingerprint TEXT DEFAULT '',
			indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (file_kind, file_path)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

func countSymbols(t *testing.T, db *sql.DB, path string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM of_symbols WHERE file_path = ?`, path).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestArtifactSaveIndexesSymbols(t *testing.T) {
	db := setupSymbolDB(t)
	body := "export function renderHeader() {\n  return 1;\n}\n"
	if _, _, err := ArtifactSave(db, &Artifact{
		Kind: "file", Path: "js/app.js", Body: body, ContentType: "application/javascript",
	}, "task-1"); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := countSymbols(t, db, "js/app.js"); got == 0 {
		t.Fatal("expected symbols indexed on save, got 0")
	}
	var line int
	if err := db.QueryRow(`SELECT line FROM of_symbols WHERE name = 'renderHeader' AND kind = 'js_func'`).Scan(&line); err != nil {
		t.Fatalf("renderHeader not indexed: %v", err)
	}
	if line != 1 {
		t.Errorf("renderHeader line = %d, want 1", line)
	}

	// Re-saving the identical body must not churn: the index state short-circuits.
	var fpBefore string
	_ = db.QueryRow(`SELECT fingerprint FROM of_symbol_index_state WHERE file_path = 'js/app.js'`).Scan(&fpBefore)
	if _, _, err := ArtifactSave(db, &Artifact{
		Kind: "file", Path: "js/app.js", Body: body, ContentType: "application/javascript",
	}, "task-1"); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	var fpAfter string
	_ = db.QueryRow(`SELECT fingerprint FROM of_symbol_index_state WHERE file_path = 'js/app.js'`).Scan(&fpAfter)
	if fpBefore != fpAfter {
		t.Errorf("fingerprint changed on unchanged re-save: %q -> %q", fpBefore, fpAfter)
	}

	// Deleting the artifact removes its symbols.
	if _, err := ArtifactDelete(db, "file", "js/app.js", "task-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := countSymbols(t, db, "js/app.js"); got != 0 {
		t.Errorf("symbols not removed on delete: %d remain", got)
	}
}

func TestBackfillStaleSymbols(t *testing.T) {
	db := setupSymbolDB(t)
	// Insert an of_files row directly, bypassing the save hook, to simulate a
	// project that predates the index.
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, storage, content_type, is_deleted, updated_at)
		VALUES ('file', 'css/site.css', '.header { color: red; }', 'inline', 'text/css', 0, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := countSymbols(t, db, "css/site.css"); got != 0 {
		t.Fatalf("expected unindexed, got %d", got)
	}
	n, remaining, err := BackfillStaleSymbols(db, 25)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Errorf("backfilled %d files, want 1", n)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0 (only one stale file, budget ample)", remaining)
	}
	if got := countSymbols(t, db, "css/site.css"); got == 0 {
		t.Error("backfill did not index the seeded file")
	}
}

func TestBackfillStaleSymbols_BudgetLeavesRemaining(t *testing.T) {
	db := setupSymbolDB(t)
	// Seed three unindexed files but allow only one per call: the first call
	// indexes one and reports two still stale, so a lookup can warn it's partial.
	for _, p := range []string{"css/a.css", "css/b.css", "css/c.css"} {
		if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, storage, content_type, is_deleted, updated_at)
			VALUES ('file', ?, '.x { color: red; }', 'inline', 'text/css', 0, CURRENT_TIMESTAMP)`, p); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	n, remaining, err := BackfillStaleSymbols(db, 1)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Errorf("indexed %d, want 1 (budget)", n)
	}
	if remaining != 2 {
		t.Errorf("remaining = %d, want 2", remaining)
	}
}
