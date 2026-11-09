package public

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"openfabrica/db/models"
)

func setupLayoutDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, body TEXT,
		is_deleted INTEGER DEFAULT 0,
		updated_at TEXT
	)`)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return db
}

func insertLayout(t *testing.T, db *sql.DB, path, body string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO of_files (kind, path, body, updated_at) VALUES ('layout', ?, ?, ?)`,
		path, body, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("insert layout %q: %v", path, err)
	}
}

func TestLoadLayout_ExactMatch(t *testing.T) {
	db := setupLayoutDB(t)
	insertLayout(t, db, "default", "<html><body>DEFAULT {{content}}</body></html>")
	insertLayout(t, db, "admin", "<html><body>ADMIN {{content}}</body></html>")
	got := loadLayout(db, "admin")
	if got == nil || !strings.Contains(got.Template, "ADMIN") {
		t.Fatalf("expected admin layout, got %+v", got)
	}
}

func TestLoadLayout_FallsBackToDefault(t *testing.T) {
	db := setupLayoutDB(t)
	insertLayout(t, db, "default", "<html><body>DEFAULT {{content}}</body></html>")
	got := loadLayout(db, "missing")
	if got == nil || !strings.Contains(got.Template, "DEFAULT") {
		t.Fatalf("expected default fallback, got %+v", got)
	}
}

func TestLoadLayout_FallsBackToOnlyLayout(t *testing.T) {
	db := setupLayoutDB(t)
	insertLayout(t, db, "main", "<html><body>MAIN {{content}}</body></html>")
	got := loadLayout(db, "default")
	if got == nil || !strings.Contains(got.Template, "MAIN") {
		t.Fatalf("expected only-layout fallback, got %+v", got)
	}
}

func TestLoadLayout_ReturnsNilWhenNoLayouts(t *testing.T) {
	db := setupLayoutDB(t)
	if got := loadLayout(db, "default"); got != nil {
		t.Fatalf("expected nil with empty DB, got %+v", got)
	}
}

func TestLoadLayout_PrefersDefaultOverOlder(t *testing.T) {
	db := setupLayoutDB(t)
	insertLayout(t, db, "main", "<html><body>MAIN {{content}}</body></html>")
	time.Sleep(2 * time.Millisecond)
	insertLayout(t, db, "default", "<html><body>DEFAULT {{content}}</body></html>")
	got := loadLayout(db, "missing")
	if got == nil || !strings.Contains(got.Template, "DEFAULT") {
		t.Fatalf("expected default to win over main, got %+v", got)
	}
}

func TestLoadLayout_IgnoresDeletedLayouts(t *testing.T) {
	db := setupLayoutDB(t)
	_, err := db.Exec(
		`INSERT INTO of_files (kind, path, body, is_deleted, updated_at) VALUES ('layout','main','<html>{{content}}</html>',1,?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := loadLayout(db, "default"); got != nil {
		t.Fatalf("expected nil when only layout is soft-deleted, got %+v", got)
	}
}

func TestStripOwnedHeadTags(t *testing.T) {
	layout := `<!DOCTYPE html><html><head>` +
		`<meta charset="UTF-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1.0">` +
		`<title>Image Editor</title>` +
		`<link rel="stylesheet" href="/files/css/editor.css">` +
		`</head><body>{{content}}<svg><title>icon</title></svg></body></html>`

	got := stripOwnedHeadTags(layout)

	if strings.Contains(got, "<title>Image Editor</title>") {
		t.Error("layout <title> in <head> should be stripped")
	}
	if strings.Contains(got, "charset") {
		t.Error("layout charset meta should be stripped")
	}
	if strings.Contains(got, "viewport") {
		t.Error("layout viewport meta should be stripped")
	}
	// The server-owned tags are gone, but everything else in the head survives.
	if !strings.Contains(got, `href="/files/css/editor.css"`) {
		t.Error("layout stylesheet link should be preserved")
	}
	// An SVG <title> lives in the body, outside <head>, and must not be touched.
	if !strings.Contains(got, "<svg><title>icon</title></svg>") {
		t.Error("SVG <title> in body should be preserved")
	}
}

func TestInterpolateRouteParams(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		params map[string]string
		want   string
	}{
		{"no params", "About Us", nil, "About Us"},
		{"static title ignores params", "Blog", map[string]string{"slug": "hi"}, "Blog"},
		{"single placeholder", "Post {slug}", map[string]string{"slug": "my-first-post"}, "Post my-first-post"},
		{"multiple placeholders", "{category}/{id}", map[string]string{"category": "tech", "id": "7"}, "tech/7"},
		{"unmatched placeholder left as-is", "Post {slug}", map[string]string{"id": "7"}, "Post {slug}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := interpolateRouteParams(tc.title, tc.params); got != tc.want {
				t.Errorf("interpolateRouteParams(%q, %v) = %q, want %q", tc.title, tc.params, got, tc.want)
			}
		})
	}
}

// manage_seo set_meta stores og.title / og.description as their own columns; they
// must reach the og tags, while twitter and the page title keep the page's own values.
func TestRenderDocument_OGOverrides(t *testing.T) {
	db := setupLayoutDB(t)
	if _, err := db.Exec(`CREATE TABLE of_seo_meta (
		page_path TEXT PRIMARY KEY, title TEXT DEFAULT '', description TEXT DEFAULT '',
		og_title TEXT DEFAULT '', og_description TEXT DEFAULT '', og_image TEXT DEFAULT '',
		canonical_url TEXT DEFAULT '', structured_data TEXT DEFAULT '', twitter_card TEXT DEFAULT '',
		locale TEXT DEFAULT ''
	)`); err != nil {
		t.Fatalf("create of_seo_meta: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_seo_meta (page_path, title, description, og_title, og_description)
		VALUES ('/', 'Page title', 'Page description', 'Share title', 'Share description')`); err != nil {
		t.Fatalf("insert seo: %v", err)
	}
	h := &Handler{}
	doc := h.renderDocument(documentSpec{
		Project:   &models.Project{Name: "p"},
		ProjectDB: db,
		PagePath:  "/",
		Content:   "<p>hi</p>",
	})
	for _, want := range []string{
		`property="og:title" content="Share title"`,
		`property="og:description" content="Share description"`,
		`name="description" content="Page description"`,
		`name="twitter:title" content="Page title"`,
		`<title>Page title</title>`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("rendered head is missing %s:\n%s", want, doc)
		}
	}
}

// A layout's {{include:name}} must resolve like a page's does. The layout gate
// accepts includes, so leaving them unresolved ships the literal placeholder.
func TestRenderDocument_ResolvesLayoutIncludes(t *testing.T) {
	db := setupLayoutDB(t)
	insertLayout(t, db, "default", "<html><head>{{assets}}</head><body>{{include:snow}}<main>{{content}}</main></body></html>")
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body) VALUES ('component', 'snow', '<canvas id="snowCanvas"></canvas>')`); err != nil {
		t.Fatalf("insert component: %v", err)
	}
	h := &Handler{}
	doc := h.renderDocument(documentSpec{
		Project:   &models.Project{Name: "p"},
		ProjectDB: db,
		PagePath:  "/",
		Content:   "<p>hi</p>",
	})
	if strings.Contains(doc, "{{include:") {
		t.Fatalf("layout include left unresolved:\n%s", doc)
	}
	if !strings.Contains(doc, `<canvas id="snowCanvas"></canvas>`) {
		t.Fatalf("component markup missing from rendered layout:\n%s", doc)
	}
}
