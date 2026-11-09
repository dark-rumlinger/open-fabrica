package public

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupParamPagesDB(t *testing.T, paths ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, body TEXT,
		status TEXT DEFAULT 'published', is_deleted INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_files: %v", err)
	}
	// Insert in the given order so row order is deterministic for the test.
	for _, p := range paths {
		if _, err := db.Exec(
			`INSERT INTO of_files (kind, path, status, is_deleted) VALUES ('page', ?, 'published', 0)`, p,
		); err != nil {
			t.Fatalf("insert %q: %v", p, err)
		}
	}
	return db
}

// A static segment must beat a ':param' at the same position, regardless of the
// order the templates were saved in. This is the fix for static blog routes
// (/about, /archive, /dashboard) being shadowed by the post route /:user/:slug.
func TestFindParamPage_StaticBeatsParam(t *testing.T) {
	cases := []struct {
		name      string
		paths     []string // insertion order (the post route deliberately first)
		request   string
		wantRoute string
		wantParam map[string]string
	}{
		{
			name:      "about not shadowed by slug",
			paths:     []string{"/:username", "/:username/:slug", "/:username/about", "/:username/archive"},
			request:   "/elena-marsh/about",
			wantRoute: "/:username/about",
			wantParam: map[string]string{"username": "elena-marsh"},
		},
		{
			name:      "archive not shadowed by slug",
			paths:     []string{"/:username/:slug", "/:username/archive"},
			request:   "/elena-marsh/archive",
			wantRoute: "/:username/archive",
			wantParam: map[string]string{"username": "elena-marsh"},
		},
		{
			name:      "dashboard not shadowed by slug",
			paths:     []string{"/:username/:slug", "/:username/dashboard"},
			request:   "/bob/dashboard",
			wantRoute: "/:username/dashboard",
			wantParam: map[string]string{"username": "bob"},
		},
		{
			name:      "a real post still resolves to the slug route",
			paths:     []string{"/:username/:slug", "/:username/about"},
			request:   "/elena-marsh/hello-world",
			wantRoute: "/:username/:slug",
			wantParam: map[string]string{"username": "elena-marsh", "slug": "hello-world"},
		},
		{
			name:      "single param route",
			paths:     []string{"/:username", "/:username/:slug"},
			request:   "/alice",
			wantRoute: "/:username",
			wantParam: map[string]string{"username": "alice"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := setupParamPagesDB(t, c.paths...)
			route, params, err := findParamPage(db, c.request)
			if err != nil {
				t.Fatalf("findParamPage(%q): %v", c.request, err)
			}
			if route != c.wantRoute {
				t.Fatalf("route = %q, want %q", route, c.wantRoute)
			}
			for k, v := range c.wantParam {
				if params[k] != v {
					t.Fatalf("param %q = %q, want %q (all: %v)", k, params[k], v, params)
				}
			}
			if len(params) != len(c.wantParam) {
				t.Fatalf("params = %v, want exactly %v", params, c.wantParam)
			}
		})
	}
}

// No matching template is still a clean miss (so the caller serves 404).
func TestFindParamPage_NoMatch(t *testing.T) {
	db := setupParamPagesDB(t, "/:username/:slug")
	if _, _, err := findParamPage(db, "/a/b/c"); err == nil {
		t.Fatal("expected no match for a deeper path")
	}
}
