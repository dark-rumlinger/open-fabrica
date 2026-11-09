package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupAuthGateDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE of_auth_endpoints (id INTEGER PRIMARY KEY, path TEXT)`,
		`CREATE TABLE of_api_endpoints (
			id INTEGER PRIMARY KEY, path TEXT, table_name TEXT,
			requires_auth INTEGER DEFAULT 0, public_read INTEGER DEFAULT 0, public_write INTEGER DEFAULT 0,
			owner_column TEXT DEFAULT ''
		)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	return db
}

func addAPIEndpoint(t *testing.T, db *sql.DB, path string, requiresAuth, publicRead int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO of_api_endpoints (path, table_name, requires_auth, public_read) VALUES (?, ?, ?, ?)`,
		path, path, requiresAuth, publicRead); err != nil {
		t.Fatalf("insert api %s: %v", path, err)
	}
}

func TestProjectHasAuth(t *testing.T) {
	db := setupAuthGateDB(t)
	if ProjectHasAuth(db) {
		t.Error("no auth endpoints -> ProjectHasAuth should be false")
	}
	if _, err := db.Exec(`INSERT INTO of_auth_endpoints (path) VALUES ('login')`); err != nil {
		t.Fatal(err)
	}
	if !ProjectHasAuth(db) {
		t.Error("an auth endpoint exists -> ProjectHasAuth should be true")
	}
}

func TestAuthGatedEndpointsWithoutAuth(t *testing.T) {
	db := setupAuthGateDB(t)
	addAPIEndpoint(t, db, "rooms", 1, 1)  // requires auth, public read (writes 401)
	addAPIEndpoint(t, db, "public", 0, 0) // public - never flagged
	addAPIEndpoint(t, db, "secure", 1, 0) // requires auth, private read+write

	// No login surface -> both auth-gated endpoints are flagged, sorted by path.
	got := AuthGatedEndpointsWithoutAuth(db)
	if len(got) != 2 {
		t.Fatalf("want 2 findings (rooms, secure), got %d: %+v", len(got), got)
	}
	if got[0].Path != "rooms" || !got[0].PublicRead {
		t.Errorf("first finding wrong: %+v (want rooms, publicRead=true)", got[0])
	}
	if got[1].Path != "secure" || got[1].PublicRead {
		t.Errorf("second finding wrong: %+v (want secure, publicRead=false)", got[1])
	}

	// Add a login -> no findings (writes can now be authenticated).
	if _, err := db.Exec(`INSERT INTO of_auth_endpoints (path) VALUES ('login')`); err != nil {
		t.Fatal(err)
	}
	if got := AuthGatedEndpointsWithoutAuth(db); got != nil {
		t.Errorf("with auth present, want no findings, got %+v", got)
	}
}

// A public_write endpoint (contact-form shape) is never flagged even with no
// login: its writes are intentionally open, so they do NOT 401 - and flagging it
// would only push the gated admin reads open, re-introducing a leak.
func TestAuthGatedEndpointsWithoutAuth_PublicWriteExcluded(t *testing.T) {
	db := setupAuthGateDB(t)
	// requires_auth=1, public_read=0, public_write=1: writes open, reads gated.
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, requires_auth, public_read, public_write) VALUES ('messages','messages',1,0,1)`); err != nil {
		t.Fatal(err)
	}
	if got := AuthGatedEndpointsWithoutAuth(db); got != nil {
		t.Errorf("public_write endpoint should be excluded, got %+v", got)
	}
}

// An owner-scoped endpoint carries its owner_column through the finding so the
// validator can prefer "add a login" over the row-leaking public downgrade.
func TestAuthGatedEndpointsWithoutAuth_CapturesOwnerColumn(t *testing.T) {
	db := setupAuthGateDB(t)
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, requires_auth, public_read, owner_column) VALUES ('models','models',1,1,'user_id')`); err != nil {
		t.Fatal(err)
	}
	got := AuthGatedEndpointsWithoutAuth(db)
	if len(got) != 1 || got[0].OwnerColumn != "user_id" {
		t.Fatalf("want one finding with owner_column=user_id, got %+v", got)
	}
}
