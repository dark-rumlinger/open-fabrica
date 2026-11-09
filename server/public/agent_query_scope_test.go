package public

import (
	"context"
	"strings"
	"testing"

	"openfabrica/db"
)

// The agent's query_data tool runs SQL the MODEL wrote, so it cannot be trusted to
// filter on the caller. Tables behind an owner- or tenant-scoped endpoint must be
// withheld from an ordinary caller, while a project key and the admin role (the two
// callers REST also exempts) keep full access.

func agentScopeProject(t *testing.T) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	stmts := []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, user_id INTEGER, body TEXT)`,
		`CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT)`,
		`INSERT INTO notes (user_id, body) VALUES (1, 'mine'), (2, 'theirs')`,
		`INSERT INTO posts (title) VALUES ('hello')`,
		`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, owner_column)
		 VALUES ('notes', 'notes', '["GET"]', 1, 'user_id')`,
		`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth)
		 VALUES ('posts', 'posts', '["GET"]', 0)`,
	}
	for _, s := range stmts {
		if _, err := pdb.Writer().Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return pdb
}

func runAgentQuery(t *testing.T, pdb *db.ProjectDB, full bool, sqlText string) string {
	t.Helper()
	tool := agentDataQueryTool(pdb, full)
	out, err := tool.Invoke(context.Background(), `{"sql":`+quoteJSON(sqlText)+`}`)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return out
}

func quoteJSON(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func TestAgentQueryWithholdsScopedTables(t *testing.T) {
	pdb := agentScopeProject(t)

	got := runAgentQuery(t, pdb, false, `SELECT body FROM notes`)
	if !strings.HasPrefix(got, "error:") || strings.Contains(got, "theirs") {
		t.Fatalf("an ordinary caller read an owner-scoped table: %s", got)
	}
	// Quoting and case do not hide the table.
	for _, q := range []string{`SELECT body FROM "notes"`, `select body from NOTES`, `SELECT * FROM posts WHERE id IN (SELECT id FROM notes)`} {
		if got := runAgentQuery(t, pdb, false, q); !strings.HasPrefix(got, "error:") {
			t.Errorf("%q reached a scoped table: %s", q, got)
		}
	}

	if got := runAgentQuery(t, pdb, false, `SELECT title FROM posts`); !strings.Contains(got, "hello") {
		t.Errorf("an unscoped table must stay queryable, got %s", got)
	}

	if got := runAgentQuery(t, pdb, true, `SELECT body FROM notes ORDER BY id`); !strings.Contains(got, "theirs") {
		t.Errorf("a project key or admin keeps full access, got %s", got)
	}
}

func TestAgentQueryDescriptionHidesScopedTables(t *testing.T) {
	pdb := agentScopeProject(t)
	desc := agentDataQueryTool(pdb, false).Def.Description
	if strings.Contains(desc, "notes") {
		t.Errorf("scoped table named in the tool description: %s", desc)
	}
	if !strings.Contains(desc, "posts") {
		t.Errorf("unscoped table missing from the tool description: %s", desc)
	}
	if full := agentDataQueryTool(pdb, true).Def.Description; !strings.Contains(full, "notes") {
		t.Errorf("full-access description should list every table: %s", full)
	}
}
