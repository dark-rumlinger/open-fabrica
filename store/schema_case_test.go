package store

import "testing"

// SQLite resolves identifiers case-insensitively, so the of_ namespace / reserved
// guards must fold before matching - otherwise a reducer or DDL path could reach a
// platform table via "OF_ws_endpoints" / "MIGRATIONS".
func TestIsSystemTableCaseInsensitive(t *testing.T) {
	cases := map[string]bool{
		"of_ws_endpoints": true,
		"OF_ws_endpoints": true,
		"Of_Ws_Endpoints": true,
		"of_users":        true,
		"migrations":      true,
		"Migrations":      true,
		"MIGRATIONS":      true,
		"world_edits":     false,
		"players":         false, // a legit user table that folds to a non-reserved name
	}
	for name, want := range cases {
		if got := IsSystemTable(name); got != want {
			t.Errorf("IsSystemTable(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSanitizedTableNameRejectsUppercaseSystem(t *testing.T) {
	for _, name := range []string{"OF_ws_endpoints", "OF_ws_room_tokens", "Of_dynamic_tables", "MIGRATIONS"} {
		if _, err := SanitizedTableName(name); err == nil {
			t.Errorf("SanitizedTableName(%q) should be rejected as a system table", name)
		}
	}
	// A user table with capitals is still accepted (only of_/migrations are reserved).
	if _, err := SanitizedTableName("Players"); err != nil {
		t.Errorf("SanitizedTableName(%q) should be accepted: %v", "Players", err)
	}
}
