package store

import "testing"

// TestArtifactSaveScopeDefault covers ArtifactSave's inherit-then-infer scope
// resolution: new JS/CSS code files default to "page", other assets to "global",
// explicit scope wins, and a re-save keeps the existing scope (the sticky rule
// preventing a plain re-save from flipping a linked page asset back to global).
func TestArtifactSaveScopeDefault(t *testing.T) {
	db := setupArtifactDB(t)

	scopeOf := func(path string) string {
		t.Helper()
		var s *string
		if err := db.QueryRow(`SELECT scope FROM of_files WHERE kind = 'file' AND path = ? AND is_deleted = 0`, path).Scan(&s); err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		if s == nil {
			return "<nil>"
		}
		return *s
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"js infers page", "js/main.js", "page"},
		{"mjs infers page", "js/util.mjs", "page"},
		{"css infers page", "css/app.css", "page"},
		{"svg infers global", "favicon.svg", "global"},
		{"json infers global", "data/config.json", "global"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Artifact{Kind: "file", Path: c.path, Body: "x"}
			if _, _, err := ArtifactSave(db, a, "test"); err != nil {
				t.Fatalf("save: %v", err)
			}
			if got := scopeOf(c.path); got != c.want {
				t.Errorf("scope = %q, want %q", got, c.want)
			}
		})
	}

	// Explicit scope wins over inference: a JS file the model marks global stays
	// global (the rare genuinely-sitewide helper).
	explicit := &Artifact{Kind: "file", Path: "js/analytics.js", Body: "x", Scope: "global"}
	if _, _, err := ArtifactSave(db, explicit, "test"); err != nil {
		t.Fatalf("save explicit: %v", err)
	}
	if got := scopeOf("js/analytics.js"); got != "global" {
		t.Errorf("explicit scope = %q, want global", got)
	}

	// Sticky: re-saving an existing artifact with no scope keeps the prior scope.
	// js/main.js inferred "page" above; a plain re-save must NOT flip it back to
	// "global".
	resave := &Artifact{Kind: "file", Path: "js/main.js", Body: "y"}
	if _, _, err := ArtifactSave(db, resave, "test"); err != nil {
		t.Fatalf("resave: %v", err)
	}
	if got := scopeOf("js/main.js"); got != "page" {
		t.Errorf("re-saved scope = %q, want page (sticky)", got)
	}

	// Sticky also holds the other way: a global asset re-saved with no scope stays
	// global, and an explicit scope on re-save still overrides.
	if _, _, err := ArtifactSave(db, &Artifact{Kind: "file", Path: "js/analytics.js", Body: "z"}, "test"); err != nil {
		t.Fatalf("resave global: %v", err)
	}
	if got := scopeOf("js/analytics.js"); got != "global" {
		t.Errorf("re-saved global scope = %q, want global (sticky)", got)
	}
	if _, _, err := ArtifactSave(db, &Artifact{Kind: "file", Path: "js/main.js", Body: "w", Scope: "global"}, "test"); err != nil {
		t.Fatalf("resave with explicit override: %v", err)
	}
	if got := scopeOf("js/main.js"); got != "global" {
		t.Errorf("explicit override on re-save = %q, want global", got)
	}
}
