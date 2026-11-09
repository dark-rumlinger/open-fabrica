package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupSuggestDB(t *testing.T, paths []string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY, kind TEXT, path TEXT, is_deleted INTEGER DEFAULT 0,
		UNIQUE(kind, path))`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, p := range paths {
		if _, err := db.Exec(`INSERT INTO of_files (kind, path) VALUES ('file', ?)`, p); err != nil {
			t.Fatalf("insert %s: %v", p, err)
		}
	}
	return db
}

func TestSuggestArtifactPaths(t *testing.T) {
	db := setupSuggestDB(t, []string{
		"js/glide.core.js", "js/glide.state.js", "js/glide.render.js",
		"css/app.css", "js/audio.js",
	})

	// Guessed a sibling by a plausible-but-wrong name: the same-stem modules rank
	// first, which is exactly the recovery the build agent needs.
	got := SuggestArtifactPaths(db, "file", "js/glide.js", 3)
	if len(got) == 0 || got[0] != "js/glide.core.js" && got[0] != "js/glide.state.js" && got[0] != "js/glide.render.js" {
		t.Fatalf("expected a js/glide.* suggestion first, got %v", got)
	}
	for _, g := range got {
		if g == "css/app.css" {
			t.Errorf("unrelated css/app.css should not be suggested for js/glide.js; got %v", got)
		}
	}

	// Same basename in a different directory is the strongest signal.
	db2 := setupSuggestDB(t, []string{"js/weather.js", "css/styles.css"})
	got2 := SuggestArtifactPaths(db2, "file", "weather.js", 3)
	if len(got2) == 0 || got2[0] != "js/weather.js" {
		t.Fatalf("expected js/weather.js as the top same-basename suggestion, got %v", got2)
	}

	// Nothing remotely close -> no noise.
	if s := SuggestArtifactPaths(db2, "file", "totally-unrelated-xyz-name.bin", 3); len(s) != 0 {
		t.Errorf("expected no suggestions for an unrelated path, got %v", s)
	}

	// Hint formatting.
	if h := SuggestArtifactPathsHint(db2, "file", "weather.js"); h == "" {
		t.Error("expected a non-empty did-you-mean hint")
	}
	if h := SuggestArtifactPathsHint(nil, "file", "x.js"); h != "" {
		t.Errorf("nil db must yield empty hint, got %q", h)
	}
}

func TestResolveUniqueBasename(t *testing.T) {
	// A flat state leaf referenced with a phantom js/ prefix. The
	// basename is unique across the project, so it resolves to the real path.
	db := setupSuggestDB(t, []string{"tetris.state.js", "tetris.audio.js", "tetris.engine.js"})
	if got, ok := ResolveUniqueBasename(db, "file", "js/tetris.state.js"); !ok || got != "tetris.state.js" {
		t.Fatalf("expected resolve to tetris.state.js, got %q ok=%v", got, ok)
	}

	// The reverse direction: a flat guess for a file that actually lives under js/.
	db2 := setupSuggestDB(t, []string{"js/app.js", "css/app.css"})
	if got, ok := ResolveUniqueBasename(db2, "file", "app.js"); !ok || got != "js/app.js" {
		t.Fatalf("expected resolve to js/app.js, got %q ok=%v", got, ok)
	}

	// Ambiguous: two directories share the basename -> refuse to guess, so the
	// caller falls back to the did-you-mean hint.
	db3 := setupSuggestDB(t, []string{"js/util.js", "vendor/util.js"})
	if got, ok := ResolveUniqueBasename(db3, "file", "util.js"); ok {
		t.Fatalf("ambiguous basename must not resolve, got %q", got)
	}

	// No file shares the basename -> no resolution (a genuinely missing file).
	if got, ok := ResolveUniqueBasename(db, "file", "js/missing.js"); ok {
		t.Fatalf("an unmatched basename must not resolve, got %q", got)
	}

	// A nil db is safe and resolves nothing.
	if _, ok := ResolveUniqueBasename(nil, "file", "x.js"); ok {
		t.Fatal("nil db must not resolve")
	}
}

func TestSuggestHelpers(t *testing.T) {
	if got := pathBaseName("js/glide.core.js"); got != "glide.core.js" {
		t.Errorf("pathBaseName: got %q", got)
	}
	if got := pathBaseName("bare.js"); got != "bare.js" {
		t.Errorf("pathBaseName bare: got %q", got)
	}
	if got := stem("glide.core.js"); got != "glide" {
		t.Errorf("stem: got %q", got)
	}
	if d := levenshteinDist("kitten", "sitting"); d != 3 {
		t.Errorf("levenshteinDist(kitten,sitting): got %d want 3", d)
	}
	if d := levenshteinDist("", "abc"); d != 3 {
		t.Errorf("levenshteinDist empty: got %d want 3", d)
	}
}
