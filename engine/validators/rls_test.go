package validators

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// setupRLSDB builds the schema the rls_scope validator reads through
// store.UnscopedOwnerEndpoints: of_api_endpoints plus the backing tables it
// introspects. notes is row-owned (user_id); tags is shared (no owner-like col).
func setupRLSDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE of_api_endpoints (
			id INTEGER PRIMARY KEY,
			path TEXT,
			table_name TEXT,
			requires_auth INTEGER DEFAULT 0,
			public_read INTEGER DEFAULT 0,
			public_write INTEGER DEFAULT 0,
			owner_column TEXT,
			tenant_column TEXT,
			read_only INTEGER DEFAULT 0
		)`,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, user_id INTEGER, body TEXT)`,
		`CREATE TABLE tags (id INTEGER PRIMARY KEY, label TEXT)`,
		// messages is collaborative: author column (user_id) plus a container
		// reference (channel_id), so its scope recommendation must lead with authored.
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, channel_id INTEGER, user_id INTEGER, body TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

func addRLSEndpoint(t *testing.T, db *sql.DB, path, table string, requiresAuth, publicRead, readOnly int, owner string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO of_api_endpoints
		(path, table_name, requires_auth, public_read, owner_column, tenant_column, read_only)
		VALUES (?, ?, ?, ?, ?, '', ?)`,
		path, table, requiresAuth, publicRead, owner, readOnly); err != nil {
		t.Fatalf("insert endpoint %s: %v", path, err)
	}
}

// An authenticated endpoint over a row-owned table with no owner_column is a
// cross-user data leak: the validator must flag it as a hard, deterministic error
// that drives the fix-pass.
func TestRLSScope_FlagsAuthUnscopedAsError(t *testing.T) {
	db := setupRLSDB(t)
	addRLSEndpoint(t, db, "notes", "notes", 1, 0, 0, "")

	issues := rlsScopeValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %+v", len(issues), issues)
	}
	iss := issues[0]
	if iss.Severity != "error" {
		t.Errorf("severity = %q, want error", iss.Severity)
	}
	if iss.Category != "rls_scope" {
		t.Errorf("category = %q, want rls_scope", iss.Category)
	}
	if iss.Signature != "notes" {
		t.Errorf("signature = %q, want notes", iss.Signature)
	}
	if iss.File != "" {
		t.Errorf("File = %q, want empty: the finding names an API resource, not a file", iss.File)
	}
	if !iss.Deterministic || iss.Speculative {
		t.Errorf("flags = {Deterministic:%v Speculative:%v}, want {true false}", iss.Deterministic, iss.Speculative)
	}
	if !strings.Contains(iss.Message, "user_id") {
		t.Errorf("message should name the owner-like column user_id: %q", iss.Message)
	}
}

// owner_column set -> runtime scopes every query -> clean build.
func TestRLSScope_ScopedIsClean(t *testing.T) {
	db := setupRLSDB(t)
	addRLSEndpoint(t, db, "notes", "notes", 1, 0, 0, "user_id")

	if issues := (rlsScopeValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("scoped endpoint must be clean, got: %+v", issues)
	}
}

// A public-read endpoint over a row-owned table MIGHT be an intentional public
// feed, so the finding must be a speculative, non-looping warning - never a
// destructive hard error that could drive the model to break a working public
// endpoint.
func TestRLSScope_PublicReadDowngradesToSpeculativeWarning(t *testing.T) {
	db := setupRLSDB(t)
	addRLSEndpoint(t, db, "notes", "notes", 0, 1, 0, "")

	issues := rlsScopeValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %+v", len(issues), issues)
	}
	iss := issues[0]
	if iss.Severity != "warning" {
		t.Errorf("severity = %q, want warning (public read may be intentional)", iss.Severity)
	}
	if !iss.Speculative {
		t.Errorf("want Speculative=true")
	}
	if iss.Deterministic {
		t.Errorf("a speculative finding must not be Deterministic")
	}
}

// A row-owned table that also carries a container reference (channel_id) is
// likely collaborative content, so a per-user owner scope would hide every other
// member's rows. The finding is a speculative warning that leads with the authored
// remedy, not a hard error that would force the breaking owner scope.
func TestRLSScope_ContainerTableLeadsWithAuthored(t *testing.T) {
	db := setupRLSDB(t)
	addRLSEndpoint(t, db, "messages", "messages", 1, 0, 0, "")

	issues := rlsScopeValidator{}.Validate(context.Background(), db, "", "")
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d: %+v", len(issues), issues)
	}
	iss := issues[0]
	if iss.Severity != "warning" || !iss.Speculative || iss.Deterministic {
		t.Errorf("flags = {Severity:%q Speculative:%v Deterministic:%v}, want {warning true false}", iss.Severity, iss.Speculative, iss.Deterministic)
	}
	if !strings.Contains(iss.Message, "auth_model=authored") {
		t.Errorf("container-table remedy must lead with authored: %q", iss.Message)
	}
	if !strings.Contains(iss.Message, "channel_id") {
		t.Errorf("message should name the container column channel_id: %q", iss.Message)
	}
}

// read_only (view-backed) endpoints and tables with no owner-like column have no
// RLS surface and must never be flagged.
func TestRLSScope_SkipsReadOnlyAndSharedTables(t *testing.T) {
	db := setupRLSDB(t)
	addRLSEndpoint(t, db, "notes-view", "notes", 0, 1, 1, "")
	addRLSEndpoint(t, db, "tags", "tags", 1, 0, 0, "")

	if issues := (rlsScopeValidator{}).Validate(context.Background(), db, "", ""); len(issues) != 0 {
		t.Fatalf("read-only and shared-table endpoints must be clean, got: %+v", issues)
	}
}
