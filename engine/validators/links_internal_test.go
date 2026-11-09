package validators

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestNormalizeInternalLink(t *testing.T) {
	cases := []struct {
		in       string
		wantRt   string
		wantKeep bool
	}{
		{"/streaks", "/streaks", true},
		{"/", "/", true},
		{"/about/", "/about", true},      // trailing slash normalized
		{"/about?tab=x", "/about", true}, // query stripped
		{"/about#frag", "/about", true},  // fragment stripped
		{"", "", false},
		{"#", "", false},
		{"#section", "", false},
		{"https://example.com", "", false},
		{"http://x", "", false},
		{"//cdn.example.com/a", "", false},
		{"mailto:a@b.com", "", false},
		{"tel:+15551234", "", false},
		{"javascript:void(0)", "", false},
		{"/api/posts", "", false},
		{"/files/app.js", "", false},
		{"/feed/posts", "", false},         // platform-served feed the contract tells the head to link
		{"/payments/webhook", "", false},   // platform-served provider callback
		{"/webhooks/stripe", "", false},    // platform-served inbound webhook
		{"/__of/notifications", "", false}, // platform-served inbox
		{"/manifest.json", "", false},      // extension-bearing: static asset, not a page
		{"/favicon.svg", "", false},
		{"/posts/:slug", "", false}, // literal template, not a navigable link
		{"about", "", false},        // not root-absolute
		{"./about", "", false},
	}
	for _, c := range cases {
		gotRt, gotKeep := normalizeInternalLink(c.in)
		if gotKeep != c.wantKeep || (gotKeep && gotRt != c.wantRt) {
			t.Errorf("normalizeInternalLink(%q) = (%q, %v), want (%q, %v)", c.in, gotRt, gotKeep, c.wantRt, c.wantKeep)
		}
	}
}

func TestRouteResolves(t *testing.T) {
	routes := map[string]bool{"/": true, "/about": true, "/posts/:slug": true}
	cases := []struct {
		route string
		want  bool
	}{
		{"/", true},
		{"/about", true},
		{"/posts/hello", true},       // matches /posts/:slug
		{"/posts/hello/edit", false}, // segment count differs
		{"/streaks", false},          // no such route
		{"/posts", false},            // shorter than template
	}
	for _, c := range cases {
		if got := routeResolves(c.route, routes); got != c.want {
			t.Errorf("routeResolves(%q) = %v, want %v", c.route, got, c.want)
		}
	}
}

// setupPageDB builds the minimal of_files schema the internal-link and PWA
// validators read.
func setupPageDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, body TEXT DEFAULT '', scope TEXT,
		is_deleted INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_files: %v", err)
	}
	return db
}

func saveKind(t *testing.T, db *sql.DB, kind, path, body string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body) VALUES (?, ?, ?)`, kind, path, body); err != nil {
		t.Fatalf("insert %s %s: %v", kind, path, err)
	}
}

// A representative failure: the shared layout hardcodes a nav tab
// linking to /streaks, but only the / page was ever built. The validator must
// flag /streaks as a dead link.
func TestInternalLinks_FlagsDeadNavTab(t *testing.T) {
	db := setupPageDB(t)
	saveKind(t, db, "page", "/", `<section>Today</section>`)
	saveKind(t, db, "layout", "default", `<nav>
		<a href="/" class="nav-tab" data-route="/">Today</a>
		<a href="/streaks" class="nav-tab" data-route="/streaks">Streaks</a>
	</nav>`)

	issues := internalLinkValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 dead_link issue, got %d: %+v", len(issues), issues)
	}
	if issues[0].Category != "dead_link" || issues[0].Page != "/streaks" || issues[0].File != "default" {
		t.Errorf("unexpected issue: %+v", issues[0])
	}
	if issues[0].Severity != "error" || !issues[0].Deterministic {
		t.Errorf("dead_link must be a deterministic error: %+v", issues[0])
	}
}

// Once the missing page is built (the fix-pass outcome), the same layout is clean.
func TestInternalLinks_CleanWhenAllRoutesExist(t *testing.T) {
	db := setupPageDB(t)
	saveKind(t, db, "page", "/", `<a href="/streaks">Streaks</a><a href="/posts/hello">A post</a>`)
	saveKind(t, db, "page", "/streaks", `<section>Streaks</section>`)
	saveKind(t, db, "page", "/posts/:slug", `<article>post</article>`)
	saveKind(t, db, "layout", "default", `<nav><a href="/">Today</a><a href="/streaks">Streaks</a></nav>`)

	if issues := (internalLinkValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("all routes exist, must be clean, got: %+v", issues)
	}
}

// A `data-route` to a not-yet-built path is a client-side view switch (an
// app-shaped SPA idiom the server never owns), not a dead server link, so it must
// NOT be flagged - only real `href`/form-action navigation is resolved. The page
// here carries a `data-route` to /settings with no saved /settings page and no
// matching href, and the validator must stay clean.
func TestInternalLinks_IgnoresClientSideViewSwitch(t *testing.T) {
	db := setupPageDB(t)
	saveKind(t, db, "page", "/", `<nav>
		<button data-route="/">Today</button>
		<button data-route="/settings">Settings</button>
	</nav>`)

	if issues := (internalLinkValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("a data-route client-side view switch must not be flagged as a dead link, got: %+v", issues)
	}
}

// External links, fragments, assets and /api or /files paths must never be flagged.
func TestInternalLinks_IgnoresNonRouteLinks(t *testing.T) {
	db := setupPageDB(t)
	saveKind(t, db, "page", "/", `
		<a href="https://example.com">ext</a>
		<a href="#top">scroll</a>
		<a href="mailto:a@b.com">mail</a>
		<a href="/api/posts">api</a>
		<link rel="stylesheet" href="/files/app.css">
		<link rel="manifest" href="/manifest.json">
		<img src="/files/og.png">
		<a href="/favicon.svg">icon</a>`)

	if issues := (internalLinkValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("non-route links must not be flagged, got: %+v", issues)
	}
}
