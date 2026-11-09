package validators

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// setupAuthPageDB builds the of_files schema the auth-page-shell validator reads,
// including the metadata column it pulls the per-page layout from.
func setupAuthPageDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE of_files (
		id INTEGER PRIMARY KEY,
		kind TEXT, path TEXT, body TEXT DEFAULT '', scope TEXT,
		metadata TEXT, is_deleted INTEGER DEFAULT 0
	)`); err != nil {
		t.Fatalf("create of_files: %v", err)
	}
	return db
}

func savePageMeta(t *testing.T, db *sql.DB, kind, path, body, metadata string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_files (kind, path, body, metadata) VALUES (?, ?, ?, ?)`,
		kind, path, body, metadata); err != nil {
		t.Fatalf("insert %s %s: %v", kind, path, err)
	}
}

const chromeLayout = `<aside class="sidebar"><nav><a href="/">Home</a></nav></aside>{{content}}`
const loginBody = `<form><input type="email"><input type="password"></form>
<script>window.redirectIfLoggedIn('/');</script>`

// The representative failure: a /login page with no layout override inherits the
// default chrome layout, so the sidebar shows before sign-in.
func TestAuthPageShell_FlagsLoginInDefaultLayout(t *testing.T) {
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "layout", "default", chromeLayout, "")
	savePageMeta(t, db, "page", "/login", loginBody, `{"title":"Sign In"}`)

	issues := authPageShellValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 auth_page_in_shell issue, got %d: %+v", len(issues), issues)
	}
	if issues[0].Category != "auth_page_in_shell" || issues[0].Page != "/login" {
		t.Errorf("unexpected issue: %+v", issues[0])
	}
	if issues[0].Severity != "error" || !issues[0].Deterministic {
		t.Errorf("auth_page_in_shell must be a deterministic error: %+v", issues[0])
	}
}

// The fix-pass outcome: the same page saved with layout:"none" is clean.
func TestAuthPageShell_CleanWhenLayoutNone(t *testing.T) {
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "layout", "default", chromeLayout, "")
	savePageMeta(t, db, "page", "/login", loginBody, `{"layout":"none"}`)

	if issues := (authPageShellValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("layout:none login page must be clean, got: %+v", issues)
	}
}

// A default layout with no nav/sidebar is already a bare frame: wrapping a login
// page in it is fine and must not be flagged.
func TestAuthPageShell_CleanWhenLayoutHasNoChrome(t *testing.T) {
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "layout", "default", `<main>{{content}}</main>`, "")
	savePageMeta(t, db, "page", "/login", loginBody, "")

	if issues := (authPageShellValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("chrome-less layout must not be flagged, got: %+v", issues)
	}
}

// An in-app change-password page (behind requireAuth, no redirectIfLoggedIn, not
// a login route) has a password field but legitimately lives inside the shell.
func TestAuthPageShell_IgnoresInAppPasswordForm(t *testing.T) {
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "layout", "default", chromeLayout, "")
	savePageMeta(t, db, "page", "/settings", `<form><input type="password" name="new"></form>
<script>window.requireAuth().then(function(u){});</script>`, "")

	if issues := (authPageShellValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("an in-app change-password page must not be flagged, got: %+v", issues)
	}
}

// Login detection also fires on the redirectIfLoggedIn signal even when the route
// is not login-ish (e.g. mounted at the project root).
func TestAuthPageShell_DetectsLoginByRedirectHelper(t *testing.T) {
	db := setupAuthPageDB(t)
	savePageMeta(t, db, "layout", "default", chromeLayout, "")
	savePageMeta(t, db, "page", "/", loginBody, "")

	issues := authPageShellValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 || issues[0].Page != "/" {
		t.Fatalf("expected the root login page flagged, got: %+v", issues)
	}
}
