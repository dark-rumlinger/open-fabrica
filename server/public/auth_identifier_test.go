package public

import (
	"strings"
	"testing"
)

// authEndpointsSchema is the of_auth_endpoints DDL shared by the identifier
// tests. Mirrors the columns loadAuthEndpoint SELECTs.
const authEndpointsSchema = `CREATE TABLE of_auth_endpoints (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	table_name TEXT, path TEXT UNIQUE,
	username_column TEXT DEFAULT 'username',
	password_column TEXT DEFAULT 'password',
	public_columns TEXT DEFAULT '[]',
	signup_columns TEXT DEFAULT '[]',
	jwt_expiry_hours INTEGER DEFAULT 24,
	default_role TEXT DEFAULT 'user',
	role_column TEXT DEFAULT 'role',
	rate_limit INTEGER DEFAULT 60,
	tenant_signup_mode TEXT DEFAULT 'none')`

// TestLoadAuthEndpointAcceptsValidIdentifiers pins that a well-formed config
// still loads (regression guard for the added table_name validation).
func TestLoadAuthEndpointAcceptsValidIdentifiers(t *testing.T) {
	db := setupRowIDColumnDB(t, authEndpointsSchema,
		`INSERT INTO of_auth_endpoints (table_name, path) VALUES ('users', 'ok')`)

	h := &Handler{deps: &Deps{}}
	ae, err := h.loadAuthEndpoint(db, "ok")
	if err != nil {
		t.Fatalf("valid config should load, got error: %v", err)
	}
	if ae.TableName != "users" {
		t.Fatalf("TableName = %q, want %q", ae.TableName, "users")
	}
}

// TestLoadAuthEndpointRejectsMaliciousTableName pins that a stored table_name
// which is not a plain SQL identifier fails closed at load, so it can never
// reach the interpolated auth queries.
func TestLoadAuthEndpointRejectsMaliciousTableName(t *testing.T) {
	cases := []struct {
		name  string
		table string
	}{
		{"quote break-out", `users"; DROP TABLE users; --`},
		{"stacked statement", "users; DROP TABLE users"},
		{"hyphen not allowed", "users-table"},
		{"reserved word", "select"},
		{"leading digit", "1users"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupRowIDColumnDB(t, authEndpointsSchema,
				`INSERT INTO of_auth_endpoints (table_name, path) VALUES ('placeholder', 'bad')`)
			// Overwrite table_name with the adversarial value (parameterized so the
			// setup itself is injection-free).
			if _, err := db.Exec(`UPDATE of_auth_endpoints SET table_name = ? WHERE path = 'bad'`, tc.table); err != nil {
				t.Fatalf("seed update: %v", err)
			}

			h := &Handler{deps: &Deps{}}
			_, err := h.loadAuthEndpoint(db, "bad")
			if err == nil {
				t.Fatalf("table_name %q should be rejected, but loadAuthEndpoint succeeded", tc.table)
			}
			if !strings.Contains(err.Error(), "table_name") {
				t.Fatalf("error should mention table_name, got: %v", err)
			}
		})
	}
}
