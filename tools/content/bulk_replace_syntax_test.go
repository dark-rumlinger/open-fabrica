package content

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/store"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupBulkDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func setupBulkDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

func seedJS(t *testing.T, db dbh.Handle, path, body string) {
	t.Helper()
	_, _, err := store.ArtifactSave(db, &store.Artifact{
		Kind:        "file",
		Path:        path,
		Storage:     store.StorageInline,
		Body:        body,
		ContentType: "application/javascript",
		Scope:       "global",
		Status:      "published",
	}, "test")
	if err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// TestBulkReplace_SkipsFileItWouldBreak runs one regex across two JS files. The
// same match decapitates a function in one file (breaking it) and lands inside a
// string literal in the other (leaving it valid). The broken file must be left
// untouched and reported under "skipped", while the valid file is still written.
func TestBulkReplace_SkipsFileItWouldBreak(t *testing.T) {
	db := setupBulkDB(t)

	// Removing "export function b(){" decapitates the second function here.
	const breaks = "export function a(){ return 1; }\nexport function b(){ return 2; }\n"
	// The same text sits inside a string literal here, so removing it is fine.
	const safe = "export const KEY = 'export function b(){';\n"

	seedJS(t, db, "js/breaks.js", breaks)
	seedJS(t, db, "js/safe.js", safe)

	ctx := &tools.ToolContext{DB: db}
	res, err := artifactActionBulkReplace(ctx, &ArtBulkReplace{Pattern: `export function b\(\)\{`, PathPrefix: "js/"})
	if err != nil {
		t.Fatalf("bulk replace returned error: %v", err)
	}
	if !res.Success {
		t.Fatalf("bulk replace failed: %s", res.Error)
	}

	data := res.Data.(map[string]any)
	if got := data["files_skipped"]; got != 1 {
		t.Errorf("files_skipped = %v, want 1", got)
	}
	if got := data["files_changed"]; got != 1 {
		t.Errorf("files_changed = %v, want 1", got)
	}

	// The breaking file was not modified.
	a, err := store.ArtifactGet(db, "file", "js/breaks.js")
	if err != nil || a == nil {
		t.Fatalf("get js/breaks.js: %v", err)
	}
	if a.Body != breaks {
		t.Errorf("js/breaks.js was modified despite the syntax gate:\n%s", a.Body)
	}

	// The safe file was rewritten.
	b, err := store.ArtifactGet(db, "file", "js/safe.js")
	if err != nil || b == nil {
		t.Fatalf("get js/safe.js: %v", err)
	}
	if b.Body != "export const KEY = '';\n" {
		t.Errorf("js/safe.js body = %q, want the string-literal match removed", b.Body)
	}
}
