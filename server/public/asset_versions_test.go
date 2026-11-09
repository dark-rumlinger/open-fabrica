package public

import (
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/store"
)

// A patched module must get a new URL, or a browser that cached the old one (the
// /files max-age is a day) keeps running the pre-fix code: the probe reported the
// same exception after every fix, and the live site served stale JS. Only the
// patched module's URL may change, so the rest of the page stays cached.
func TestAssetVersions_PatchChangesOnlyThatFilesURL(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	save := func(path, body string) {
		t.Helper()
		a := &store.Artifact{Kind: "file", Path: path, Body: body, ContentType: "application/javascript"}
		if _, _, err := store.ArtifactSave(pdb.Writer(), a, "brain"); err != nil {
			t.Fatalf("save %s: %v", path, err)
		}
	}
	save("js/app.js", "export const a = 1;\n")
	save("js/util.js", "export const u = 1;\n")
	before := loadAssetVersions(pdb.Reader())
	if before["js/app.js"] == "" || before["js/util.js"] == "" {
		t.Fatalf("every live file needs a version suffix, got %v", before)
	}

	// Same length and, most likely, the same second: the history version still moves.
	save("js/app.js", "export const a = 2;\n")
	after := loadAssetVersions(pdb.Reader())
	if after["js/app.js"] == before["js/app.js"] {
		t.Errorf("patched module kept its URL %q, so a cached copy is served", after["js/app.js"])
	}
	if after["js/util.js"] != before["js/util.js"] {
		t.Errorf("untouched module's URL changed (%q -> %q), busting its cache for nothing", before["js/util.js"], after["js/util.js"])
	}

	entries := projectModuleImportEntries([]string{"js/app.js", "js/util.js"}, after)
	if got := entries["app"]; !strings.HasPrefix(got, "/files/js/app.js?v=") || got != after.url("js/app.js") {
		t.Errorf("importmap[app] = %q, want the versioned URL %q", got, after.url("js/app.js"))
	}
}
