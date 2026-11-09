package auth

import (
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/store"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// TestAuthCreateEmitsBootstrap pins that create_auth emits the platform-managed
// browser auth bootstrap (js/auth.js) as a global-scope, non-editable file with
// this endpoint's path substituted for __AUTH_PATH__ (no placeholder left). The
// load-order guarantees that prevent the /admin <-> /login redirect loop are
// thereby frozen in a platform-owned file instead of retyped per build. Uses a
// fully-migrated project DB so the artifact path (and its history) exist.
func TestAuthCreateEmitsBootstrap(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	w := pdb.Writer()

	// Register a user table (schema + secure password column) and create the
	// physical table, so create_auth's preconditions hold. role is in the schema
	// so create_auth does not try to ALTER a column that already exists.
	if _, err := w.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		VALUES ('users', '{"email":"TEXT","password":"TEXT","role":"TEXT"}', '{"password":"hash"}')`); err != nil {
		t.Fatalf("register users table: %v", err)
	}
	if _, err := w.Exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT, password TEXT, role TEXT DEFAULT '')`); err != nil {
		t.Fatalf("create users table: %v", err)
	}

	ctx := &tools.ToolContext{DB: w}
	res, err := (&AuthCreate{EndpointPath: "auth", TableName: "users", UsernameColumn: "email"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create_auth: err=%v res=%+v", err, res)
	}

	a, err := store.ArtifactGet(pdb.Reader(), "file", "js/auth.js")
	if err != nil || a == nil {
		t.Fatalf("auth bootstrap js/auth.js not emitted: %v", err)
	}
	if a.Scope != "global" {
		t.Errorf("bootstrap scope = %q, want global (so it auto-injects into every page)", a.Scope)
	}
	if !store.IsPlatformManagedArtifact(pdb.Reader(), "file", "js/auth.js") {
		t.Error("bootstrap is not flagged platform_managed (the model could then edit/delete it)")
	}
	if strings.Contains(a.Body, "__AUTH_PATH__") {
		t.Error("bootstrap still contains the __AUTH_PATH__ placeholder (path not substituted)")
	}
	if !strings.Contains(a.Body, "/api/auth/me") {
		t.Error("bootstrap did not substitute the auth path into the /me fetch")
	}
}
