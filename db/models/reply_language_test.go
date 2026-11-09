package models

import (
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
)

func openMigrated(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestUserAssistantLanguage(t *testing.T) {
	database := openMigrated(t)
	u, err := CreateUser(database.DB, "ada", "hash", "admin", "Ada")
	if err != nil {
		t.Fatal(err)
	}

	got, err := GetUserAssistantLanguage(database.DB, u.ID)
	if err != nil || got != "" {
		t.Fatalf("new user: got %q, %v; want the default", got, err)
	}

	for _, want := range []string{"es", "match", ""} {
		if err := SetUserAssistantLanguage(database.DB, u.ID, want); err != nil {
			t.Fatalf("set %q: %v", want, err)
		}
		got, err := GetUserAssistantLanguage(database.DB, u.ID)
		if err != nil || got != want {
			t.Fatalf("after set %q: got %q, %v", want, got, err)
		}
	}
}

func TestUserAssistantLanguageIsPerUser(t *testing.T) {
	database := openMigrated(t)
	a, _ := CreateUser(database.DB, "ada", "hash", "admin", "Ada")
	b, _ := CreateUser(database.DB, "bob", "hash", "member", "Bob")
	if err := SetUserAssistantLanguage(database.DB, a.ID, "de"); err != nil {
		t.Fatal(err)
	}
	if got, _ := GetUserAssistantLanguage(database.DB, b.ID); got != "" {
		t.Errorf("another user's preference leaked: %q", got)
	}
}

func TestSetUserAssistantLanguageRejectsUnknownUser(t *testing.T) {
	database := openMigrated(t)
	if err := SetUserAssistantLanguage(database.DB, 9999, "es"); err == nil {
		t.Error("an orphan preference row was accepted")
	}
}

func TestDeletingAUserRemovesTheirPreference(t *testing.T) {
	database := openMigrated(t)
	u, _ := CreateUser(database.DB, "ada", "hash", "admin", "Ada")
	if err := SetUserAssistantLanguage(database.DB, u.ID, "fr"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteUser(database.DB, u.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.DB.QueryRow("SELECT COUNT(*) FROM user_preferences").Scan(&n); err != nil || n != 0 {
		t.Errorf("preference rows left after the user was deleted: %d, %v", n, err)
	}
}

func TestProjectReplyLanguage(t *testing.T) {
	d, id := newProjectConfigDB(t)

	p, _ := GetProjectByID(d, id)
	if got := ProjectReplyLanguage(p); got != "" {
		t.Fatalf("fresh project: got %q", got)
	}

	// Another stored key must survive every change.
	if err := SetProjectUserFlows(d, id, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"es", "match", "ja"} {
		if err := SetProjectReplyLanguage(d, id, want); err != nil {
			t.Fatalf("set %q: %v", want, err)
		}
		p, _ = GetProjectByID(d, id)
		if got := ProjectReplyLanguage(p); got != want {
			t.Fatalf("after set %q: got %q", want, got)
		}
		if !ProjectUserFlowsEnabled(p) {
			t.Fatalf("setting %q dropped an unrelated config key", want)
		}
	}

	if err := SetProjectReplyLanguage(d, id, ""); err != nil {
		t.Fatal(err)
	}
	p, _ = GetProjectByID(d, id)
	if got := ProjectReplyLanguage(p); got != "" {
		t.Errorf("clearing left %q", got)
	}
	if !ProjectUserFlowsEnabled(p) {
		t.Error("clearing dropped an unrelated config key")
	}
}

func TestSetProjectReplyLanguageSkipsUnchangedWrites(t *testing.T) {
	d, id := newProjectConfigDB(t)
	if err := SetProjectReplyLanguage(d, id, "fr"); err != nil {
		t.Fatal(err)
	}
	// Pin updated_at so a rewrite would be visible.
	if _, err := d.Exec("UPDATE projects SET updated_at = '2000-01-01 00:00:00' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	if err := SetProjectReplyLanguage(d, id, "fr"); err != nil {
		t.Fatal(err)
	}
	var stamp string
	if err := d.QueryRow("SELECT updated_at FROM projects WHERE id = ?", id).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stamp, "2000-01-01") {
		t.Errorf("an unchanged directive rewrote the project row: updated_at=%s", stamp)
	}
}

func TestSetProjectReplyLanguageUnknownProject(t *testing.T) {
	d, _ := newProjectConfigDB(t)
	if err := SetProjectReplyLanguage(d, 9999, "es"); err == nil {
		t.Error("expected an error for a project that does not exist")
	}
}
