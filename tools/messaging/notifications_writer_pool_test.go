package messaging

import (
	"testing"
	"time"

	"openfabrica/internal/dbtest"
	"openfabrica/tools"
)

// notifFindUsersByRole resolves the auth endpoint, then queries that endpoint's
// table for matching users. Both steps run on ctx.DB, the single-connection
// writer pool, so the endpoint lookup must not hold a cursor while the user
// query runs.
//
// A cursor makes this deadlock even when the loop returns on the first row: the
// returned call is evaluated before the deferred Close, so the connection is
// still held.
func TestNotifFindUsersByRole_NoDeadlockOnRealWriterPool(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	if _, err := pdb.ExecWrite(
		`CREATE TABLE app_users (id INTEGER PRIMARY KEY, email TEXT, role TEXT)`,
	); err != nil {
		t.Fatalf("create users table: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO app_users (id, email, role) VALUES (1,'a@x.com','admin'), (2,'b@x.com','user'), (3,'c@x.com','admin')`,
	); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_auth_endpoints (table_name, path, role_column) VALUES ('app_users', 'auth', 'role')`,
	); err != nil {
		t.Fatalf("register auth endpoint: %v", err)
	}
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	var ids []int
	dbtest.RunWithDeadlineGuard(t, 10*time.Second, "notifFindUsersByRole", func() {
		ids = notifFindUsersByRole(ctx, "admin")
	})
	if len(ids) != 2 {
		t.Fatalf("got %d admin ids (%v), want 2", len(ids), ids)
	}
}

// The no-endpoint path must stay a clean nil rather than a hang.
func TestNotifFindUsersByRole_NoAuthEndpoint(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	var ids []int
	dbtest.RunWithDeadlineGuard(t, 10*time.Second, "notifFindUsersByRole", func() {
		ids = notifFindUsersByRole(ctx, "admin")
	})
	if ids != nil {
		t.Errorf("got %v, want nil when no auth endpoint is registered", ids)
	}
}
