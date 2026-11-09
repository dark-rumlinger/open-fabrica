package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupAssetsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE of_files (id INTEGER PRIMARY KEY, kind TEXT, path TEXT, scope TEXT DEFAULT 'global', is_deleted INTEGER DEFAULT 0)`,
		`CREATE TABLE of_page_assets (id INTEGER PRIMARY KEY, page_path TEXT, file_path TEXT, position INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

// PageForAsset resolves the page a file runs on, so a probe can execute the
// file in its real context. Returns one page when attached, false when not, and
// is deterministic (lowest page_path) when attached to several.
func TestPageForAsset(t *testing.T) {
	db := setupAssetsDB(t)
	db.Exec(`INSERT INTO of_page_assets (page_path, file_path, position) VALUES ('/sail','world.js',1)`)
	db.Exec(`INSERT INTO of_page_assets (page_path, file_path, position) VALUES ('/race','world.js',0)`) // also attached here

	page, ok := PageForAsset(db, "world.js")
	if !ok {
		t.Fatal("an attached file must resolve to a page")
	}
	if page != "/race" {
		t.Errorf("expected the lowest page_path (/race) for determinism, got %q", page)
	}
	if _, ok := PageForAsset(db, "lonely.js"); ok {
		t.Error("an unattached file must report no page so the probe falls back to the home page")
	}
}
