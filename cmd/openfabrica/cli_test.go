package main

import (
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/security"
)

func newUsersDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestResetAdminPasswordForTheOnlyAdmin(t *testing.T) {
	database := newUsersDB(t)
	old, _ := security.HashPassword("old-password")
	admin, err := models.CreateUser(database.DB, "ada", old, "admin", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := models.CreateUser(database.DB, "bob", old, "member", "Bob"); err != nil {
		t.Fatal(err)
	}
	before, _ := models.GetUserTokenVersion(database.DB, admin.ID)

	user, password, err := resetAdminPassword(database.DB, "")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "ada" || len(password) != 20 {
		t.Fatalf("user=%q password length %d", user.Username, len(password))
	}
	updated, _ := models.GetUserByUsername(database.DB, "ada")
	if !security.CheckPassword(password, updated.PasswordHash) || security.CheckPassword("old-password", updated.PasswordHash) {
		t.Error("the new password did not replace the old one")
	}
	if after, _ := models.GetUserTokenVersion(database.DB, admin.ID); after != before+1 {
		t.Error("existing sessions must be invalidated")
	}
	if bob, _ := models.GetUserByUsername(database.DB, "bob"); !security.CheckPassword("old-password", bob.PasswordHash) {
		t.Error("another user's password was touched")
	}
}

func TestResetAdminPasswordNeedsAnUnambiguousTarget(t *testing.T) {
	database := newUsersDB(t)
	if _, _, err := resetAdminPassword(database.DB, ""); err == nil || !strings.Contains(err.Error(), "no admin") {
		t.Errorf("no admin exists: %v", err)
	}
	h, _ := security.HashPassword("x")
	models.CreateUser(database.DB, "ada", h, "admin", "")
	models.CreateUser(database.DB, "eve", h, "admin", "")
	if _, _, err := resetAdminPassword(database.DB, ""); err == nil || !strings.Contains(err.Error(), "ada") {
		t.Errorf("two admins must ask which: %v", err)
	}
	if _, _, err := resetAdminPassword(database.DB, "eve"); err != nil {
		t.Errorf("naming the user works: %v", err)
	}
	if _, _, err := resetAdminPassword(database.DB, "nobody"); err == nil {
		t.Error("an unknown user must be an error")
	}
}

func TestGeneratedPasswordsAreDistinctAndReadable(t *testing.T) {
	a, _ := generatePassword(20)
	b, _ := generatePassword(20)
	if a == b {
		t.Error("two generated passwords matched")
	}
	if strings.ContainsAny(a, "0O1lI") {
		t.Errorf("look-alike characters in %q", a)
	}
}
