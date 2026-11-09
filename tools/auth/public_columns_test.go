package auth

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// TestParsePublicColumns pins that public_columns accepts both the documented
// string form and the {"name":...} object form the model sometimes emits, and
// that a malformed element fails loudly rather than being silently dropped (a
// silent drop can empty public_columns, so /me omits the role column and
// requireRole bounces every logged-in user home).
func TestParsePublicColumns(t *testing.T) {
	cols, errMsg := parsePublicColumns([]any{
		"id",
		map[string]any{"name": "username"},
		map[string]any{"name": "role"},
	})
	if errMsg != "" {
		t.Fatalf("unexpected error parsing valid columns: %s", errMsg)
	}
	if got := strings.Join(cols, ","); got != "id,username,role" {
		t.Fatalf("cols = %q, want id,username,role", got)
	}

	for _, bad := range []any{
		map[string]any{"nope": "role"}, // object without "name"
		map[string]any{"name": ""},     // empty name
		42,                             // neither string nor object
		"bad name!",                    // invalid column name
	} {
		if _, errMsg := parsePublicColumns([]any{bad}); errMsg == "" {
			t.Errorf("parsePublicColumns(%v) returned no error, want a loud failure", bad)
		}
	}
}

// TestAuthCreateRepairsRegistryDriftedRoleColumn: a role column that exists in
// the physical table but is missing from the schema_def registry must succeed
// with a registry repair, not fail with a contradictory "does not exist ...
// duplicate column name" error (which drove real 5-call retry loops).
func TestAuthCreateRepairsRegistryDriftedRoleColumn(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	w := pdb.Writer()
	// The registry omits "role"; the physical table already has it.
	if _, err := w.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		VALUES ('staff', '{"username":"TEXT","password":"TEXT"}', '{"password":"hash"}')`); err != nil {
		t.Fatalf("register staff table: %v", err)
	}
	if _, err := w.Exec(`CREATE TABLE staff (
		id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, password TEXT, role TEXT DEFAULT '')`); err != nil {
		t.Fatalf("create staff table: %v", err)
	}

	ctx := &tools.ToolContext{DB: w}
	res, err := (&AuthCreate{
		EndpointPath:   "staffauth",
		TableName:      "staff",
		UsernameColumn: "username",
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		errText := ""
		if res != nil {
			errText = res.Error
		}
		t.Fatalf("create_auth should repair registry drift, got err=%v toolErr=%q", err, errText)
	}

	var schemaDef string
	if err := pdb.Reader().QueryRow(
		"SELECT schema_def FROM of_dynamic_tables WHERE table_name='staff'").Scan(&schemaDef); err != nil {
		t.Fatalf("read schema_def: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(schemaDef), &m); err != nil {
		t.Fatalf("schema_def unparseable: %q (%v)", schemaDef, err)
	}
	if _, ok := m["role"]; !ok {
		t.Errorf("schema_def should carry the repaired role column, got %s", schemaDef)
	}
}

// TestAuthCreateAcceptsObjectPublicColumns is the end-to-end check: an object-form
// public_columns must land in of_auth_endpoints as a real string array. If it
// serialized to "null" instead, /me would fall back to id+username (no role) and
// break role-gating.
func TestAuthCreateAcceptsObjectPublicColumns(t *testing.T) {
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

	ctx := &tools.ToolContext{DB: w}
	res, err := (&AuthCreate{
		EndpointPath:   "auth",
		TableName:      "users",
		UsernameColumn: "username",
		PublicColumns: []any{
			map[string]any{"name": "id"},
			map[string]any{"name": "username"},
			map[string]any{"name": "role"},
		},
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create_auth: err=%v res=%+v", err, res)
	}

	var stored string
	if err := pdb.Reader().QueryRow(
		"SELECT public_columns FROM of_auth_endpoints WHERE path='auth'").Scan(&stored); err != nil {
		t.Fatalf("read public_columns: %v", err)
	}
	var names []string
	if err := json.Unmarshal([]byte(stored), &names); err != nil {
		t.Fatalf("public_columns is not a JSON string array: %q (%v)", stored, err)
	}
	if strings.Join(names, ",") != "id,username,role" {
		t.Fatalf("stored public_columns = %v, want [id username role] - object form was dropped", names)
	}
}
