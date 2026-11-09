package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// setupLinkDB builds the minimal schema store.UnlinkedPageArtifacts reads:
// of_files (bodies included, for import-graph parsing) and of_page_assets.
func setupLinkDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE of_files (
			id INTEGER PRIMARY KEY,
			kind TEXT, path TEXT, body TEXT, scope TEXT,
			is_deleted INTEGER DEFAULT 0
		)`,
		`CREATE TABLE of_page_assets (
			page_path TEXT, file_path TEXT, position INTEGER DEFAULT 0
		)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

func saveFile(t *testing.T, db *sql.DB, path, body string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, scope, body) VALUES ('file', ?, 'page', ?)`, path, body); err != nil {
		t.Fatalf("insert %s: %v", path, err)
	}
}

func attachFile(t *testing.T, db *sql.DB, page, path string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_page_assets (page_path, file_path) VALUES (?, ?)`, page, path); err != nil {
		t.Fatalf("attach %s: %v", path, err)
	}
}

// The orphan-leaf failure shape: an entry that exports state to leaf modules
// but never imports them back. The leaves are reached by no linked entry, so the
// browser never loads them - the validator must flag every one as an error.
func TestLinkReadiness_FlagsOrphanLeaves(t *testing.T) {
	db := setupLinkDB(t)
	// Entry imports only an external lib; it never imports the feature leaves.
	saveFile(t, db, "files/builder.js", `import * as THREE from 'three';`)
	// Leaves import FROM the entry (the inverted direction) but nothing imports
	// THEM, so they are orphaned.
	saveFile(t, db, "files/palette.js", `import { state } from './builder.js';`)
	saveFile(t, db, "files/dragdrop.js", `import { state } from './builder.js';`)
	saveFile(t, db, "files/manipulation.js", `import { state } from './builder.js';`)
	attachFile(t, db, "/", "files/builder.js") // only the entry is linked

	issues := linkReadinessValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 3 {
		t.Fatalf("expected 3 orphan issues, got %d: %+v", len(issues), issues)
	}
	got := map[string]bool{}
	for _, iss := range issues {
		if iss.Severity != "error" {
			t.Errorf("issue %s: severity = %q, want error", iss.File, iss.Severity)
		}
		if iss.Category != "orphaned_module" {
			t.Errorf("issue %s: category = %q, want orphaned_module", iss.File, iss.Category)
		}
		got[iss.File] = true
	}
	for _, want := range []string{"files/palette.js", "files/dragdrop.js", "files/manipulation.js"} {
		if !got[want] {
			t.Errorf("missing orphan issue for %s", want)
		}
	}
}

// A correctly wired graph - the entry statically imports each leaf - must pass
// clean. This is the pattern the fix-pass should drive the build toward.
func TestLinkReadiness_WiredGraphIsClean(t *testing.T) {
	db := setupLinkDB(t)
	saveFile(t, db, "files/builder.js", `
		import './palette.js';
		import './dragdrop.js';
	`)
	saveFile(t, db, "files/palette.js", `export const p = 1;`)
	saveFile(t, db, "files/dragdrop.js", `export const d = 1;`)
	attachFile(t, db, "/", "files/builder.js")

	if issues := (linkReadinessValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("wired graph must be clean, got: %+v", issues)
	}
}

// A representative regression: leaves loaded via a dynamic
// `import(path)` loop are reached by no static import, but they load fine at
// runtime (the server serves them by path). The validator must NOT emit a
// destructive hard error - it must downgrade to a speculative, non-looping
// warning whose message drops the "delete it" guidance, so a false orphan flag
// can never drive the model to tear out a working dynamic loader.
func TestLinkReadiness_OpaqueDynamicImportDowngradesToWarning(t *testing.T) {
	db := setupLinkDB(t)
	saveFile(t, db, "space/main.js", `
		import { state } from './state.js';
		const leaves = [['planet', './planet.js'], ['multitool', './multitool.js']];
		for (const [name, path] of leaves) { const mod = await import(path); register(name, mod.default); }
	`)
	saveFile(t, db, "space/state.js", `export const state = {};`)
	saveFile(t, db, "space/planet.js", `export default {};`)
	saveFile(t, db, "space/multitool.js", `export default {};`)
	attachFile(t, db, "/", "space/main.js")

	issues := linkReadinessValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 2 {
		t.Fatalf("expected 2 orphan issues, got %d: %+v", len(issues), issues)
	}
	for _, iss := range issues {
		if iss.Category != "orphaned_module" {
			t.Errorf("issue %s: category = %q, want orphaned_module", iss.File, iss.Category)
		}
		if iss.Severity != "warning" {
			t.Errorf("issue %s: severity = %q, want warning (opaque dynamic import in closure)", iss.File, iss.Severity)
		}
		if !iss.Speculative {
			t.Errorf("issue %s: want Speculative=true", iss.File)
		}
		if iss.Deterministic {
			t.Errorf("issue %s: a speculative finding must not be Deterministic", iss.File)
		}
		if strings.Contains(strings.ToLower(iss.Message), "delete it") {
			t.Errorf("issue %s: speculative message must not carry destructive 'delete it' remediation: %q", iss.File, iss.Message)
		}
	}
}

// An unlinked stylesheet (no module can import CSS) is flagged for direct attach,
// not as an import-graph orphan.
func TestLinkReadiness_FlagsUnlinkedCSS(t *testing.T) {
	db := setupLinkDB(t)
	saveFile(t, db, "files/app.js", `export const x = 1;`)
	saveFile(t, db, "css/extra.css", `.x{}`)
	attachFile(t, db, "/", "files/app.js")

	issues := linkReadinessValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 || issues[0].File != "css/extra.css" || issues[0].Category != "unlinked_stylesheet" {
		t.Fatalf("expected one unlinked_stylesheet issue for css/extra.css, got: %+v", issues)
	}
}
