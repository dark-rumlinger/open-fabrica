package public

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func modulesOnlyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, body TEXT, metadata TEXT DEFAULT '{}', scope TEXT DEFAULT 'global',
		is_deleted INTEGER DEFAULT 0, updated_at TEXT DEFAULT ''
	)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	return db
}

// Every project JS file is an ES module: the renderer emits
// <script type="module"> for ALL global JS regardless of metadata - the
// is_module flag is gone from the model-facing world and a plain .js with no
// import/export still runs strict, matching the static analysis.
func TestAutoInjectAssets_AllJSAreModules(t *testing.T) {
	db := modulesOnlyTestDB(t)
	seed := func(path, meta string) {
		t.Helper()
		if _, err := db.Exec(
			`INSERT INTO of_files (kind, path, body, metadata, scope) VALUES ('file', ?, 'x', ?, 'global')`,
			path, meta,
		); err != nil {
			t.Fatalf("insert %q: %v", path, err)
		}
	}
	seed("js/app.js", `{}`)                     // no flag at all
	seed("js/legacy.js", `{"is_module":false}`) // stale explicit false is ignored
	seed("js/helper.mjs", `{}`)

	_, js, _ := autoInjectAssets(db, nil)
	for _, p := range []string{"js/app.js", "js/legacy.js", "js/helper.mjs"} {
		if !strings.Contains(js, `<script type="module" src="/files/`+p+`"`) {
			t.Errorf("%s should get a module tag, got: %q", p, js)
		}
	}
	if strings.Contains(js, "<script src=") {
		t.Errorf("no classic <script> tags may remain: %q", js)
	}
}

// The service worker is registered by the browser via
// navigator.serviceWorker.register, never loaded through a page tag or
// imported - it must be excluded from tag injection and the module map.
func TestServiceWorkerExcludedFromTagsAndImportmap(t *testing.T) {
	db := modulesOnlyTestDB(t)
	for _, p := range []string{"service-worker.js", "js/app.js"} {
		if _, err := db.Exec(
			`INSERT INTO of_files (kind, path, body, scope) VALUES ('file', ?, 'x', 'global')`, p,
		); err != nil {
			t.Fatalf("insert %q: %v", p, err)
		}
	}
	_, js, _ := autoInjectAssets(db, nil)
	if strings.Contains(js, "service-worker.js") {
		t.Errorf("service worker must not get a script tag: %q", js)
	}
	for _, p := range projectModuleFilePaths(db) {
		if p == "service-worker.js" {
			t.Error("service worker must not be an importmap module")
		}
	}
}

// The importmap covers EVERY project JS file by logical bare name - there is no
// is_module filter left, so a file saved with no metadata is importable by its
// siblings immediately.
func TestProjectModuleFilePaths_AllJSIncluded(t *testing.T) {
	db := modulesOnlyTestDB(t)
	for _, p := range []string{"js/world.js", "js/state.js"} {
		if _, err := db.Exec(
			`INSERT INTO of_files (kind, path, body, scope) VALUES ('file', ?, 'x', 'page')`, p,
		); err != nil {
			t.Fatalf("insert %q: %v", p, err)
		}
	}
	paths := projectModuleFilePaths(db)
	if len(paths) != 2 {
		t.Fatalf("expected both JS files as modules, got %v", paths)
	}
	entries := projectModuleImportEntries(paths, nil)
	if entries["world"] != "/files/js/world.js" {
		t.Errorf(`importmap["world"] = %q, want "/files/js/world.js"`, entries["world"])
	}
}
