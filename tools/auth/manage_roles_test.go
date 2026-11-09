package auth

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

// admin is built in by name: the gates and the RLS bypass key on it, so assign_role
// accepts it on a fresh project. Anything else must be created first, and the error
// says so instead of promising roles that were never seeded.
func TestAssignRole_AdminIsBuiltInOthersMustExist(t *testing.T) {
	pdb := seedUsersAuth(t, 0)
	w := pdb.Writer()
	if _, err := w.Exec(`INSERT INTO users (username, password) VALUES ('ann','x')`); err != nil {
		t.Fatal(err)
	}
	ctx := &tools.ToolContext{DB: w}

	res, err := (&RoleAssign{Table: "users", UserID: 1, Role: "admin", RoleColumn: "role"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("assign_role admin on a fresh project must work: %v %+v", err, res)
	}
	var role string
	_ = w.QueryRow(`SELECT role FROM users WHERE id=1`).Scan(&role)
	if role != "admin" {
		t.Fatalf("role = %q, want admin", role)
	}

	res, _ = (&RoleAssign{Table: "users", UserID: 1, Role: "editor", RoleColumn: "role"}).Run(ctx)
	if res.Success || !strings.Contains(res.Error, "create it first") {
		t.Fatalf("an uncreated role must be refused: %+v", res)
	}
}

// The role column default is the auth endpoint's configured column, not a literal.
func TestAssignRole_DefaultsRoleColumnFromAuthEndpoint(t *testing.T) {
	pdb := seedUsersAuth(t, 0)
	w := pdb.Writer()
	if _, err := w.Exec(`ALTER TABLE users ADD COLUMN access TEXT DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`UPDATE of_auth_endpoints SET role_column='access' WHERE table_name='users'`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(`INSERT INTO users (username, password) VALUES ('ann','x')`); err != nil {
		t.Fatal(err)
	}
	// RoleColumn "role" is what the arg default fills in when the model omits it.
	res, err := (&RoleAssign{Table: "users", UserID: 1, Role: "admin", RoleColumn: "role"}).Run(&tools.ToolContext{DB: w})
	if err != nil || !res.Success {
		t.Fatalf("assign_role: %v %+v", err, res)
	}
	var access string
	_ = w.QueryRow(`SELECT access FROM users WHERE id=1`).Scan(&access)
	if access != "admin" {
		t.Fatalf("the configured role column was not written: access=%q", access)
	}
}
