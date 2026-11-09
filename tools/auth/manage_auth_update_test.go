package auth

import (
	"testing"

	"openfabrica/db"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// seedUsersAuth creates a users table + auth endpoint and returns the project DB.
func seedUsersAuth(t *testing.T, rateLimit int) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	w := pdb.Writer()
	if _, err := w.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		VALUES ('users', '{"username":"TEXT","password":"TEXT","role":"TEXT"}', '{"password":"hash"}')`); err != nil {
		t.Fatalf("register users table: %v", err)
	}
	if _, err := w.Exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, password TEXT, role TEXT DEFAULT '')`); err != nil {
		t.Fatalf("create users table: %v", err)
	}
	res, err := (&AuthCreate{
		EndpointPath:   "auth",
		TableName:      "users",
		UsernameColumn: "username",
		RateLimit:      rateLimit,
	}).Run(&tools.ToolContext{DB: w})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create_auth: err=%v res=%+v", err, res)
	}
	return pdb
}

// TestAuthCreatePersistsRateLimit pins that create stores the configured
// rate_limit, and that omitting it falls back to 60 (the guard that stands in
// for the struct-tag default when the handler is called directly).
func TestAuthCreatePersistsRateLimit(t *testing.T) {
	// Explicit value round-trips.
	pdb := seedUsersAuth(t, 30)
	var got int
	if err := pdb.Reader().QueryRow(
		"SELECT rate_limit FROM of_auth_endpoints WHERE path='auth'").Scan(&got); err != nil {
		t.Fatalf("read rate_limit: %v", err)
	}
	if got != 30 {
		t.Fatalf("rate_limit = %d, want 30", got)
	}

	// Omitted value defaults to 60.
	pdb2 := seedUsersAuth(t, 0)
	if err := pdb2.Reader().QueryRow(
		"SELECT rate_limit FROM of_auth_endpoints WHERE path='auth'").Scan(&got); err != nil {
		t.Fatalf("read rate_limit: %v", err)
	}
	if got != 60 {
		t.Fatalf("default rate_limit = %d, want 60", got)
	}
}

// TestAuthUpdateAppliesColumnsAndRateLimit asserts that an update persists the
// username_column, password_column and rate_limit args AuthUpdate advertises,
// rather than accepting them and dropping them on the floor.
func TestAuthUpdateAppliesColumnsAndRateLimit(t *testing.T) {
	pdb := seedUsersAuth(t, 60)
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	res, err := (&AuthUpdate{
		EndpointPath:   "auth",
		UsernameColumn: "email",
		PasswordColumn: "pwd",
		RateLimit:      5,
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("update_auth: err=%v res=%+v", err, res)
	}

	var username, password string
	var rateLimit int
	if err := pdb.Reader().QueryRow(
		"SELECT username_column, password_column, rate_limit FROM of_auth_endpoints WHERE path='auth'").
		Scan(&username, &password, &rateLimit); err != nil {
		t.Fatalf("read back endpoint: %v", err)
	}
	if username != "email" {
		t.Errorf("username_column = %q, want email", username)
	}
	if password != "pwd" {
		t.Errorf("password_column = %q, want pwd", password)
	}
	if rateLimit != 5 {
		t.Errorf("rate_limit = %d, want 5", rateLimit)
	}
}

// TestAuthUpdateRejectsInvalidColumnName pins that a malformed column name is
// rejected rather than spliced into an UPDATE.
func TestAuthUpdateRejectsInvalidColumnName(t *testing.T) {
	pdb := seedUsersAuth(t, 60)
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	res, _ := (&AuthUpdate{EndpointPath: "auth", UsernameColumn: "bad name!"}).Run(ctx)
	if res == nil || res.Success {
		t.Fatalf("update with invalid username_column should fail, got res=%+v", res)
	}
}
