package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// setupAuthGateDB builds the minimal schema the auth_gate validator reads through
// store.AuthGatedEndpointsWithoutAuth: of_auth_endpoints (the login surface) plus
// of_api_endpoints (requires_auth / public_read).
func setupAuthGateDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE of_auth_endpoints (id INTEGER PRIMARY KEY, path TEXT)`,
		`CREATE TABLE of_api_endpoints (id INTEGER PRIMARY KEY, path TEXT, table_name TEXT,
			requires_auth INTEGER DEFAULT 0, public_read INTEGER DEFAULT 0, public_write INTEGER DEFAULT 0,
			owner_column TEXT DEFAULT '')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

// An auth-gated endpoint in a project with no login is a hard error: every write
// 401s from the browser, so the validator must drive the fix-pass.
func TestAuthGate_FlagsAuthGatedWithoutLogin(t *testing.T) {
	db := setupAuthGateDB(t)
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, requires_auth, public_read) VALUES ('rooms','rooms',1,1)`); err != nil {
		t.Fatal(err)
	}
	issues := authGateValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %+v", len(issues), issues)
	}
	iss := issues[0]
	if iss.Severity != "error" || iss.Category != "auth_gate" || iss.Signature != "rooms" || !iss.Deterministic {
		t.Errorf("issue = %+v, want error/auth_gate/rooms/deterministic", iss)
	}
	if iss.File != "" {
		t.Errorf("File = %q, want empty: the finding names an API resource, not a file", iss.File)
	}
	if !strings.Contains(iss.Message, "login") {
		t.Errorf("message should explain the missing login: %q", iss.Message)
	}
}

// An owner-scoped endpoint with no login gets the OWNER-aware remedy: prefer adding
// a login, and DO NOT silently flip to public (which would discard per-user
// ownership and expose every row to every visitor).
func TestAuthGate_OwnerScopedPrefersAuthNotPublic(t *testing.T) {
	db := setupAuthGateDB(t)
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, requires_auth, public_read, owner_column) VALUES ('models','models',1,1,'user_id')`); err != nil {
		t.Fatal(err)
	}
	issues := authGateValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %+v", len(issues), issues)
	}
	msg := issues[0].Message
	if !strings.Contains(msg, "owner-scoped") || !strings.Contains(msg, "user_id") {
		t.Errorf("owner-scoped remedy should name the ownership and column: %q", msg)
	}
	if !strings.Contains(msg, "PREFER adding an auth surface") {
		t.Errorf("owner-scoped remedy should prefer a login: %q", msg)
	}
	if !strings.Contains(msg, "world-writable") {
		t.Errorf("owner-scoped remedy should warn that public leaks every row: %q", msg)
	}
}

// A login surface exists -> writes can be authenticated -> clean.
func TestAuthGate_CleanWithLogin(t *testing.T) {
	db := setupAuthGateDB(t)
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, requires_auth) VALUES ('rooms','rooms',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_auth_endpoints (path) VALUES ('login')`); err != nil {
		t.Fatal(err)
	}
	if issues := (authGateValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("with a login, want clean, got: %+v", issues)
	}
}

// A public endpoint (requires_auth=0) is never flagged.
func TestAuthGate_PublicEndpointClean(t *testing.T) {
	db := setupAuthGateDB(t)
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, requires_auth) VALUES ('rooms','rooms',0)`); err != nil {
		t.Fatal(err)
	}
	if issues := (authGateValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("public endpoint must be clean, got: %+v", issues)
	}
}
