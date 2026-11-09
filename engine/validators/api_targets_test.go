package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// setupAPITargetDB builds the spine + CRUD + of_files schema the api_target
// validator reads.
func setupAPITargetDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE of_endpoints (
			id INTEGER PRIMARY KEY, path TEXT NOT NULL, sub_segment TEXT NOT NULL DEFAULT '',
			method TEXT NOT NULL DEFAULT '*', kind TEXT NOT NULL
		)`,
		`CREATE TABLE of_api_endpoints (id INTEGER PRIMARY KEY, path TEXT)`,
		`CREATE TABLE of_files (
			id INTEGER PRIMARY KEY,
			kind TEXT, path TEXT, body TEXT DEFAULT '', scope TEXT,
			metadata TEXT, is_deleted INTEGER DEFAULT 0
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

func claimAPITarget(t *testing.T, db *sql.DB, path, sub, method, kind string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES (?,?,?,?)`,
		path, sub, method, kind); err != nil {
		t.Fatalf("claim %s: %v", path, err)
	}
}

func saveAPITargetFile(t *testing.T, db *sql.DB, path, body string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, metadata) VALUES ('file', ?, ?, '')`, path, body); err != nil {
		t.Fatalf("insert file %s: %v", path, err)
	}
}

func apiTargetIssues(t *testing.T, db *sql.DB) []Issue {
	t.Helper()
	return apiTargetValidator{}.Validate(context.Background(), db, "", "")
}

// TestAPITarget_DeadPathFlagged is the case nothing else catches: the page loads,
// the probe is green, and the feature silently does nothing.
func TestAPITarget_DeadPathFlagged(t *testing.T) {
	db := setupAPITargetDB(t)
	claimAPITarget(t, db, "items", "", "GET", "crud")
	saveAPITargetFile(t, db, "/files/app.js", `
		const { rows } = await fetch('/api/typo').then(r => r.json());
		render(rows);`)

	issues := apiTargetIssues(t, db)
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1: %+v", len(issues), issues)
	}
	if !strings.Contains(issues[0].Message, "/api/typo") {
		t.Errorf("message should name the dead target; got %q", issues[0].Message)
	}
	if !strings.Contains(issues[0].Message, "/api/items") {
		t.Errorf("message should list the registered paths so the fix is one turn; got %q", issues[0].Message)
	}
	if issues[0].Severity != "warning" || !issues[0].Speculative {
		t.Errorf("must be a speculative warning, never a fix-pass driver; got severity=%q speculative=%v", issues[0].Severity, issues[0].Speculative)
	}
	if issues[0].Deterministic {
		t.Error("a speculative finding must not also be deterministic")
	}
}

// TestAPITarget_LiveShapesClean is the false-positive guard, and the reason the
// check matches a segment-aligned prefix rather than resolving the full URL: the
// /api namespace is flat, and several kinds mount sub-routes or row ids under a
// base path. Every line here addresses a real endpoint and must stay silent.
func TestAPITarget_LiveShapesClean(t *testing.T) {
	db := setupAPITargetDB(t)
	claimAPITarget(t, db, "items", "", "GET", "crud")
	claimAPITarget(t, db, "files", "upload", "POST", "upload")
	claimAPITarget(t, db, "auth", "login", "*", "auth")
	claimAPITarget(t, db, "news", "push", "*", "push")
	claimAPITarget(t, db, "assistant", "chat", "POST", "llm")

	saveAPITargetFile(t, db, "/files/app.js", `
		fetch('/api/items');                       // bare CRUD list
		fetch('/api/items/5');                     // CRUD single row - no spine row of its own
		fetch('/api/items/_stats');                // a CRUD feature, not a kind
		fetch('/api/items?limit=20&offset=0');     // query string is not part of the path
		fetch('/api/files/upload', {method:'POST'});
		fetch('/api/auth/login', {method:'POST'});
		fetch('/api/news/push/subscribe', {method:'POST'});  // splits to endpointPath "news/push"
		fetch('/api/assistant/chat', {method:'POST'});
		apiFetch('items');                         // apiFetch takes a BARE path
		apiFetch('items/5');
		apiFetch('/api/items');                    // ...but uses a leading slash verbatim
	`)

	if issues := apiTargetIssues(t, db); len(issues) != 0 {
		t.Fatalf("live API shapes must stay silent, got %d: %+v", len(issues), issues)
	}
}

// TestAPITarget_BareNonCrudPathFlagged covers a non-CRUD path trap: an upload endpoint
// at path "upload" serves /api/upload/upload, so a page POSTing the bare
// /api/upload is dead even though the base path exists. The validator must flag
// the bare call while leaving the correct suffixed route clean.
func TestAPITarget_BareNonCrudPathFlagged(t *testing.T) {
	db := setupAPITargetDB(t)
	claimAPITarget(t, db, "upload", "upload", "POST", "upload")
	saveAPITargetFile(t, db, "/files/app.js", `
		fetch('/api/upload', {method:'POST', body: fd});          // dead: bare path, no /upload suffix
		fetch('/api/upload/upload', {method:'POST', body: fd});   // correct route`)

	issues := apiTargetIssues(t, db)
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1: %+v", len(issues), issues)
	}
	// The dead-target list is the text between "calls " and ", but no endpoint".
	// It must be exactly the bare /api/upload; the suffixed route is served.
	msg := issues[0].Message
	dead := msg
	if i := strings.Index(msg, " calls "); i >= 0 {
		dead = msg[i+len(" calls "):]
	}
	if i := strings.Index(dead, ", but no endpoint"); i >= 0 {
		dead = dead[:i]
	}
	if dead != "/api/upload" {
		t.Errorf("dead target should be exactly the bare /api/upload, got %q (full: %q)", dead, msg)
	}
}

// TestAPITarget_UnresolvableSkipped pins the deliberate blind spots. A target the
// validator cannot know statically is skipped in silence - guessing would bounce
// a correct build.
func TestAPITarget_UnresolvableSkipped(t *testing.T) {
	db := setupAPITargetDB(t)
	claimAPITarget(t, db, "items", "", "GET", "crud")
	saveAPITargetFile(t, db, "/files/dyn.js", "\n"+
		"fetch(`/api/${resource}/list`);\n"+
		"fetch('/api/' + resource);\n"+
		"fetch(endpointUrl);\n"+
		"fetch('https://example.com/api/other');\n"+
		"fetch('/files/logo.svg');\n")

	if issues := apiTargetIssues(t, db); len(issues) != 0 {
		t.Fatalf("dynamic/external targets must be skipped silently, got %d: %+v", len(issues), issues)
	}
}

// TestAPITarget_NoEndpointsStillFlags: a page calling /api/... in a project that created
// no endpoint at all is the worst form of the miss (every call 404s), so it is reported,
// naming that no endpoint exists.
func TestAPITarget_NoEndpointsStillFlags(t *testing.T) {
	db := setupAPITargetDB(t)
	saveAPITargetFile(t, db, "/files/app.js", `fetch('/api/anything');`)
	issues := apiTargetIssues(t, db)
	if len(issues) != 1 {
		t.Fatalf("a call into a project with no endpoints must be reported once, got %d: %+v", len(issues), issues)
	}
	if !strings.Contains(issues[0].Message, "created no API endpoint yet") {
		t.Errorf("the finding should say no endpoint exists, got %q", issues[0].Message)
	}
}

// TestAPITarget_PlatformFilesSkipped: the emitted helpers address their own
// routes and are not the model's to fix.
func TestAPITarget_PlatformFilesSkipped(t *testing.T) {
	db := setupAPITargetDB(t)
	claimAPITarget(t, db, "items", "", "GET", "crud")
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, metadata)
		VALUES ('file', 'js/auth.js', ?, '{"platform_managed":true}')`,
		`fetch('/api/whatever/me');`); err != nil {
		t.Fatalf("insert platform file: %v", err)
	}
	if issues := apiTargetIssues(t, db); len(issues) != 0 {
		t.Fatalf("platform-managed files are skipped, got %d: %+v", len(issues), issues)
	}
}

// TestAPITarget_OneIssuePerFile: several dead targets in one file are one
// finding, so a single fix-pass corrects them together.
func TestAPITarget_OneIssuePerFile(t *testing.T) {
	db := setupAPITargetDB(t)
	claimAPITarget(t, db, "items", "", "GET", "crud")
	saveAPITargetFile(t, db, "/files/app.js", `
		fetch('/api/nope');
		fetch('/api/nope');   // the same dead target twice is one target
		apiFetch('alsonope');`)

	issues := apiTargetIssues(t, db)
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1 per file: %+v", len(issues), issues)
	}
	for _, want := range []string{"/api/nope", "/api/alsonope"} {
		if !strings.Contains(issues[0].Message, want) {
			t.Errorf("message should name %s; got %q", want, issues[0].Message)
		}
	}
	if strings.Count(issues[0].Message, "/api/nope,") > 1 {
		t.Errorf("a repeated target should be listed once; got %q", issues[0].Message)
	}
}
