package triggers

import (
	"log/slog"
	"testing"

	"openfabrica/db"
	"openfabrica/events"
)

// setupRunSQLProject creates a project DB with a users table (one non-admin
// row) and an audit table, the fixtures the run_sql security tests act on.
func setupRunSQLProject(t *testing.T) (*db.ProjectDBManager, *db.ProjectDB) {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(`CREATE TABLE users (id INTEGER PRIMARY KEY, role TEXT)`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO users (id, role) VALUES (1, 'user')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pdb.ExecWrite(`CREATE TABLE audit (id INTEGER PRIMARY KEY, actor TEXT)`); err != nil {
		t.Fatalf("create audit: %v", err)
	}
	return mgr, pdb
}

func userRole(t *testing.T, pdb *db.ProjectDB) string {
	t.Helper()
	var role string
	if err := pdb.Writer().QueryRow(`SELECT role FROM users WHERE id = 1`).Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	return role
}

// TestRunSQLRejectsTemplatedSQLInjection is the core injection regression: a run_sql
// trigger whose sql interpolates {{username}} must NOT escalate privilege when
// the event payload carries a stacked-statement injection. execute() exempts the
// sql field from templating, so the literal {{...}} reaches executeRunSQL, which
// refuses it - users.role stays 'user'. Substituting the payload into the SQL text
// would run it as a second, stacked UPDATE.
func TestRunSQLRejectsTemplatedSQLInjection(t *testing.T) {
	mgr, pdb := setupRunSQLProject(t)
	r := &Runner{projectDBMgr: mgr, logger: slog.New(slog.DiscardHandler)}

	evt := events.NewEvent(events.EventType("data.insert"), 1, map[string]any{
		"username": "x'); UPDATE users SET role='admin' WHERE id=1;--",
	})
	// Templated sql is the classic footgun the fix neutralizes.
	r.execute(pdb.Writer(), evt, 0, "evil", "run_sql",
		`{"sql":"INSERT INTO audit(actor) VALUES ('{{username}}')"}`)

	if role := userRole(t, pdb); role != "user" {
		t.Fatalf("privilege escalation: users.role = %q, want \"user\"", role)
	}
	var auditCount int
	if err := pdb.Writer().QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&auditCount); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount != 0 {
		t.Fatalf("expected the templated-sql trigger to be refused, but %d audit rows were written", auditCount)
	}
}

// TestRunSQLParamsBindLiterally confirms the safe path: payload values flow
// through params and are bound, never parsed as SQL. A malicious username lands
// in audit verbatim and changes nothing else.
func TestRunSQLParamsBindLiterally(t *testing.T) {
	mgr, pdb := setupRunSQLProject(t)
	r := &Runner{projectDBMgr: mgr, logger: slog.New(slog.DiscardHandler)}

	malicious := "x'); UPDATE users SET role='admin' WHERE id=1;--"
	evt := events.NewEvent(events.EventType("data.insert"), 1, map[string]any{
		"username": malicious,
	})
	r.execute(pdb.Writer(), evt, 0, "safe", "run_sql",
		`{"sql":"INSERT INTO audit(actor) VALUES (?)","params":["{{username}}"]}`)

	if role := userRole(t, pdb); role != "user" {
		t.Fatalf("privilege escalation via params: users.role = %q", role)
	}
	var actor string
	if err := pdb.Writer().QueryRow(`SELECT actor FROM audit WHERE id = 1`).Scan(&actor); err != nil {
		t.Fatalf("read audit actor: %v", err)
	}
	if actor != malicious {
		t.Fatalf("bound param mismatch: actor = %q, want the literal payload", actor)
	}
}

// TestRunSQLVerbAllowlistAndStacking checks the structural guards directly.
func TestRunSQLVerbAllowlistAndStacking(t *testing.T) {
	mgr, pdb := setupRunSQLProject(t)
	r := &Runner{projectDBMgr: mgr, logger: slog.New(slog.DiscardHandler)}

	cases := []struct {
		name    string
		sql     string
		wantErr bool
	}{
		{"plain select", "SELECT 1", false},
		{"insert allowed", "INSERT INTO audit(actor) VALUES ('ok')", false},
		{"update allowed", "UPDATE users SET role='user' WHERE id=1", false},
		{"delete blocked", "DELETE FROM users", true},
		{"drop blocked", "DROP TABLE users", true},
		{"pragma blocked", "PRAGMA table_info(users)", true},
		{"cte wrapping delete blocked", "WITH x AS (SELECT 1) DELETE FROM users", true},
		{"stacked blocked", "SELECT 1; DROP TABLE users", true},
		{"leading comment then delete blocked", "-- harmless\nDELETE FROM users", true},
		{"semicolon inside literal allowed", "INSERT INTO audit(actor) VALUES ('a;b')", false},
		{"trailing semicolon allowed", "SELECT 1;", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := r.executeRunSQL(pdb.Writer(), map[string]any{"sql": tc.sql})
			if tc.wantErr && err == nil {
				t.Fatalf("expected %q to be rejected, got nil error", tc.sql)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected %q to be allowed, got: %v", tc.sql, err)
			}
		})
	}
}
