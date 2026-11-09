package models

import (
	"path/filepath"
	"testing"

	"openfabrica/db"
)

// Demoting a user must invalidate the JWTs they already hold: the admin guard
// compares each token's version to the stored one, so a role change that left the
// version alone would keep a demoted admin's old token working until it expired.
func TestUpdateUserRoleBumpsTokenVersion(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	u, err := CreateUser(database.DB, "ada", "hash", "admin", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	before, err := GetUserTokenVersion(database.DB, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateUserRole(database.DB, u.ID, "member"); err != nil {
		t.Fatal(err)
	}
	after, err := GetUserTokenVersion(database.DB, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Errorf("token_version %d -> %d, want a bump of 1", before, after)
	}
}
