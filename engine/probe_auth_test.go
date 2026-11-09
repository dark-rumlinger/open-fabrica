package engine

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"openfabrica/security"
)

func newProbeAuthDB(t *testing.T, withAuth, withUsers bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, `CREATE TABLE of_auth_endpoints (
		id INTEGER PRIMARY KEY, table_name TEXT, username_column TEXT, role_column TEXT)`)
	mustExec(t, db, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, role TEXT)`)
	if withAuth {
		mustExec(t, db, `INSERT INTO of_auth_endpoints (table_name, username_column, role_column)
			VALUES ('users','email','role')`)
	}
	if withUsers {
		// A member with a lower id than the admin: the privileged-role ordering
		// must still pick the admin, not the lowest id.
		mustExec(t, db, `INSERT INTO users (id, email, role) VALUES (1,'mia@x.io','member')`)
		mustExec(t, db, `INSERT INTO users (id, email, role) VALUES (2,'ada@x.io','admin')`)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func TestMintProbeAuthToken(t *testing.T) {
	jwt := security.NewJWTManager("test-secret-key", time.Hour)
	const projectID = 7

	t.Run("picks privileged user and mints a project-scoped token", func(t *testing.T) {
		db := newProbeAuthDB(t, true, true)
		tok := mintProbeAuthToken(db, jwt, projectID)
		if tok == "" {
			t.Fatal("expected a token, got empty")
		}
		claims, err := jwt.Validate(tok)
		if err != nil {
			t.Fatalf("token must validate with the same manager: %v", err)
		}
		if claims.ProjectID != projectID {
			t.Errorf("ProjectID = %d, want %d", claims.ProjectID, projectID)
		}
		if claims.UserID != 2 || claims.Role != "admin" {
			t.Errorf("picked user {id=%d role=%q}, want the admin {id=2 role=admin}", claims.UserID, claims.Role)
		}
	})

	t.Run("no auth endpoint -> empty (probe stays anonymous)", func(t *testing.T) {
		db := newProbeAuthDB(t, false, true)
		if tok := mintProbeAuthToken(db, jwt, projectID); tok != "" {
			t.Errorf("want empty token without an auth endpoint, got %q", tok)
		}
	})

	t.Run("no users -> empty", func(t *testing.T) {
		db := newProbeAuthDB(t, true, false)
		if tok := mintProbeAuthToken(db, jwt, projectID); tok != "" {
			t.Errorf("want empty token with no users, got %q", tok)
		}
	})

	t.Run("nil jwt manager -> empty", func(t *testing.T) {
		db := newProbeAuthDB(t, true, true)
		if tok := mintProbeAuthToken(db, nil, projectID); tok != "" {
			t.Errorf("want empty token with nil JWT manager, got %q", tok)
		}
	})
}
