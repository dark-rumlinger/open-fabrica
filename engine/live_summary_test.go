package engine

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// usersDDL mirrors the profile-rich auth table the trace built: required name /
// signup_date / last_active columns with no default are exactly what made the
// old three-column demo insert fail and lock the owner out.
const usersDDL = `CREATE TABLE users (
	id INTEGER PRIMARY KEY,
	email TEXT NOT NULL,
	password TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'member',
	name TEXT NOT NULL,
	account_status TEXT NOT NULL DEFAULT 'active',
	signup_date TEXT NOT NULL,
	last_active TEXT NOT NULL,
	bio TEXT
)`

func openUsersDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(usersDDL); err != nil {
		t.Fatalf("create users: %v", err)
	}
	return db
}

// requiredFillColumns must pick exactly the NOT NULL, no-default columns the
// caller does not already set (name, signup_date, last_active) - and skip the
// primary key, the defaulted columns, the nullable column, and the auth trio.
func TestRequiredFillColumns(t *testing.T) {
	db := openUsersDB(t)
	skip := map[string]bool{"email": true, "password": true, "role": true}
	got := requiredFillColumns(db, "users", skip)

	names := map[string]any{}
	for _, f := range got {
		names[f.name] = f.val
	}
	for _, want := range []string{"name", "signup_date", "last_active"} {
		if _, ok := names[want]; !ok {
			t.Errorf("required column %q not filled; got %v", want, names)
		}
	}
	for _, unwanted := range []string{"id", "account_status", "bio", "email", "password", "role"} {
		if _, ok := names[unwanted]; ok {
			t.Errorf("column %q must not be filled", unwanted)
		}
	}
	// A date-ish name gets a timestamp; a name column gets a readable string.
	if v, _ := names["signup_date"].(string); !strings.Contains(v, "T") {
		t.Errorf("signup_date placeholder should be a timestamp, got %v", names["signup_date"])
	}
	if names["name"] != "Demo User" {
		t.Errorf("name placeholder = %v, want \"Demo User\"", names["name"])
	}
}

// pickExistingAccount must prefer a privileged (admin) row so the surfaced
// credential can clear an admin-only page gate - the second half of the lockout.
func TestPickExistingAccount_PrefersPrivilegedRole(t *testing.T) {
	db := openUsersDB(t)
	insert := func(email, role string) {
		if _, err := db.Exec(
			`INSERT INTO users (email, password, role, name, signup_date, last_active) VALUES (?,?,?,?,?,?)`,
			email, "x", role, "n", "2024-01-01", "2024-01-01",
		); err != nil {
			t.Fatalf("seed %s: %v", email, err)
		}
	}
	insert("bob@acme.co", "member")
	insert("alice@acme.co", "admin")

	id, email, role, ok := pickExistingAccount(db, "users", "email", "role")
	if !ok || email != "alice@acme.co" || role != "admin" || id == 0 {
		t.Fatalf("expected the admin row, got id=%d email=%q role=%q ok=%v", id, email, role, ok)
	}
}

// With no privileged row, the first account is adopted; an empty table yields
// ok=false so the caller synthesizes one instead.
func TestPickExistingAccount_FallsBackAndEmpty(t *testing.T) {
	db := openUsersDB(t)
	if _, _, _, ok := pickExistingAccount(db, "users", "email", "role"); ok {
		t.Fatal("empty table must report ok=false")
	}
	if _, err := db.Exec(
		`INSERT INTO users (email, password, role, name, signup_date, last_active) VALUES (?,?,?,?,?,?)`,
		"only@acme.co", "x", "member", "n", "2024-01-01", "2024-01-01",
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, email, role, ok := pickExistingAccount(db, "users", "email", "role")
	if !ok || email != "only@acme.co" || role != "member" {
		t.Fatalf("expected the only row, got email=%q role=%q ok=%v", email, role, ok)
	}
}

func TestPlaceholderValue(t *testing.T) {
	cases := []struct {
		name, ctype string
		want        any
	}{
		{"age", "INTEGER", 0},
		{"score", "REAL", 0},
		{"signup_date", "TEXT", ""}, // timestamp checked separately
		{"last_active", "TEXT", ""},
		{"full_name", "TEXT", "Demo User"},
		{"city", "TEXT", "demo"},
	}
	for _, c := range cases {
		got := placeholderValue(c.name, c.ctype)
		switch c.name {
		case "signup_date", "last_active":
			if s, _ := got.(string); !strings.Contains(s, "T") {
				t.Errorf("placeholderValue(%q) = %v, want a timestamp", c.name, got)
			}
		default:
			if got != c.want {
				t.Errorf("placeholderValue(%q,%q) = %v, want %v", c.name, c.ctype, got, c.want)
			}
		}
	}
}
