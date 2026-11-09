package funcexec

import (
	"database/sql"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/internal/actionexec"
	"openfabrica/internal/funcsandbox"
	"openfabrica/internal/replaymodel"
	"openfabrica/internal/replayoutbox"
)

// applyWrites refuses any write that targets a PASSWORD/ENCRYPTED column, and
// must fail CLOSED when the secure-column projection can't be read: letting a
// model-authored handler overwrite a password hash is an auth bypass. An error
// here means a genuine database failure, so rejecting costs nothing on the happy path.
func TestApplyWritesFailsClosedWhenSecureColumnsUnreadable(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.Writer().Exec(
		`CREATE TABLE users (id INTEGER PRIMARY KEY, password TEXT)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	// Force the projection read to fail the way a corrupt/unavailable database
	// would, rather than reporting "this table has no secure columns".
	if _, err := pdb.Writer().Exec(`DROP TABLE of_dynamic_tables`); err != nil {
		t.Fatalf("drop registry: %v", err)
	}

	err = applyWrites(pdb.Reader(), pdb.Writer(), []funcsandbox.Write{{
		Op:    "insert",
		Table: "users",
		Row:   map[string]any{"password": "attacker-chosen"},
	}})
	if err == nil {
		t.Fatal("applyWrites allowed a write when the secure-column projection was unreadable")
	}

	var n int
	if qerr := pdb.Reader().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); qerr != nil {
		t.Fatalf("count: %v", qerr)
	}
	if n != 0 {
		t.Fatalf("the refused write was applied anyway: %d row(s) in users", n)
	}
}

// TestExecuteWiresLLM proves rt.LLM reaches ctx.llm through Execute: a handler
// summarizes via ctx.llm and persists the result through a declared write.
// The LLM is a fake closure - the real provider call (ResolveDefaultModel +
// Complete) is the same path the llm_call trigger exercises.
func TestExecuteWiresLLM(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.Writer().Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, text TEXT)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}

	var gotSystem string
	var gotMax int
	rt := Runtime{
		Reader: pdb.Reader(),
		Writer: pdb.Writer(),
		LLM: func(prompt, system string, maxTokens int) (string, error) {
			gotSystem, gotMax = system, maxTokens
			return "SUMMARY(" + prompt + ")", nil
		},
	}
	spec := Spec{Code: `function handler(req, ctx){
	  var s = ctx.llm("in:" + req.body.t, { system: "terse", max_tokens: 64 });
	  ctx.insert('notes', { text: s });
	  return { s: s };
	}`}

	resp, err := Execute(rt, spec, map[string]any{"body": map[string]any{"t": "hi"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(string(resp.Body), "SUMMARY(in:hi)") {
		t.Fatalf("llm result not returned in response: %s", resp.Body)
	}
	if gotSystem != "terse" || gotMax != 64 {
		t.Fatalf("llm opts not passed through: system=%q max=%d", gotSystem, gotMax)
	}
	var text string
	if err := pdb.Reader().QueryRow("SELECT text FROM notes ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if text != "SUMMARY(in:hi)" {
		t.Fatalf("llm output not persisted via declared write: %q", text)
	}
}

// secureProjectDB returns a project database holding a users table whose
// password_hash is a registered PASSWORD column and whose api_key is a
// registered ENCRYPTED one, plus an unregistered posts table. One row of each.
func secureProjectDB(t *testing.T) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	for _, stmt := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, password_hash TEXT, api_key TEXT)`,
		`INSERT INTO users (name, password_hash, api_key) VALUES ('ada', '$2a$12$notarealbcrypthash', 'ciphertext')`,
		`CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT)`,
		`INSERT INTO posts (title) VALUES ('hello')`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		   VALUES ('users', '{}', '{"password_hash":"hash","api_key":"encrypt"}')`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		   VALUES ('posts', '{}', '{}')`,
	} {
		if _, err := pdb.Writer().Exec(stmt); err != nil {
			t.Fatalf("seed (%s): %v", stmt, err)
		}
	}
	return pdb
}

// queryVia runs one ctx.query through a handler and returns the response body.
func queryVia(t *testing.T, pdb *db.ProjectDB, sql string) (string, error) {
	t.Helper()
	rt := Runtime{Reader: pdb.Reader(), Writer: pdb.Writer()}
	spec := Spec{Code: `function handler(req, ctx){ return { rows: ctx.query(` + "`" + sql + "`" + `, []) }; }`}
	resp, err := Execute(rt, spec, map[string]any{})
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

// A bcrypt hash that reaches a handler can be returned verbatim in its response
// body, so a PASSWORD column must never survive ctx.query. Naming the column is
// refused outright - including behind an alias, and including in a WHERE clause
// that never selects it, which would otherwise be a blind cracking oracle.
// ctx.query must accept exactly the read-only shapes the build-time manage_sql path
// accepts and refuse exactly what it refuses. A first-token check rejected a valid
// `WITH ... SELECT`, so a CTE the model verified at build time failed only once it
// ran inside a deployed handler.
func TestGuardReadOnlySelectVerbs(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		wantReject bool
	}{
		{"plain select", "SELECT id FROM orders", false},
		{"values", "VALUES (1), (2)", false},
		{"read-only cte", "WITH recent AS (SELECT * FROM orders) SELECT * FROM recent", false},
		{"recursive cte", "WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 5) SELECT n FROM seq", false},
		{"delete", "DELETE FROM orders", true},
		{"cte-wrapped delete", "WITH doomed AS (SELECT id FROM orders) DELETE FROM orders WHERE id IN (SELECT id FROM doomed)", true},
		{"cte-wrapped update", "WITH src AS (SELECT 1 AS v) UPDATE orders SET total = (SELECT v FROM src)", true},
		{"stacked", "SELECT 1; DROP TABLE orders", true},
		{"system table", "SELECT * FROM of_secrets", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, msg := guardReadOnlySelect(tc.sql, nil)
			if tc.wantReject && msg == "" {
				t.Errorf("expected %q to be rejected, got none", tc.sql)
			}
			if !tc.wantReject && msg != "" {
				t.Errorf("expected %q to pass, got %q", tc.sql, msg)
			}
		})
	}
}

func TestQueryRefusesPasswordColumnReference(t *testing.T) {
	pdb := secureProjectDB(t)
	for _, tc := range []struct{ name, sql string }{
		{"selected", `SELECT password_hash FROM users`},
		{"aliased", `SELECT password_hash AS x FROM users`},
		{"oracle", `SELECT id FROM users WHERE password_hash LIKE 'a%'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := queryVia(t, pdb, tc.sql)
			if err == nil {
				t.Fatalf("ctx.query allowed a PASSWORD column reference: %s", body)
			}
			if !strings.Contains(err.Error(), "password_hash") {
				t.Fatalf("rejection does not name the offending column: %v", err)
			}
		})
	}
}

// SELECT * names no column, so the reference guard cannot see it. The result
// strip is what keeps a PASSWORD column out of the rows either way.
func TestQueryStripsPasswordColumnFromWildcard(t *testing.T) {
	pdb := secureProjectDB(t)
	body, err := queryVia(t, pdb, `SELECT * FROM users`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(body, "password_hash") || strings.Contains(body, "bcrypt") {
		t.Fatalf("SELECT * returned a PASSWORD column: %s", body)
	}
	if !strings.Contains(body, "ada") {
		t.Fatalf("the strip removed non-secure columns too: %s", body)
	}
	// ENCRYPTED columns stay: they read back as ciphertext, which is not a
	// disclosure, and the REST path returns them decrypted by design.
	if !strings.Contains(body, "ciphertext") {
		t.Fatalf("an ENCRYPTED column was stripped: %s", body)
	}
}

// A table with no secure columns is untouched on both layers.
func TestQueryUnaffectedWithoutSecureColumns(t *testing.T) {
	pdb := secureProjectDB(t)
	body, err := queryVia(t, pdb, `SELECT id, title FROM posts`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("plain query did not return its rows: %s", body)
	}
}

// The read guard fails CLOSED for the same reason the write guard does: with no
// projection the code cannot tell whether a query touches a secret, so it must
// refuse rather than serve the rows.
func TestQueryFailsClosedWhenSecureColumnsUnreadable(t *testing.T) {
	pdb := secureProjectDB(t)
	if _, err := pdb.Writer().Exec(`DROP TABLE of_dynamic_tables`); err != nil {
		t.Fatalf("drop registry: %v", err)
	}
	body, err := queryVia(t, pdb, `SELECT id, title FROM posts`)
	if err == nil {
		t.Fatalf("ctx.query served rows when the secure-column projection was unreadable: %s", body)
	}
}

// TestExecuteLLMUnavailable: with no LLM in the Runtime, ctx.llm throws and the
// call surfaces an error (mapped to a 500 / failed run by the caller).
func TestExecuteLLMUnavailable(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)

	rt := Runtime{Reader: pdb.Reader(), Writer: pdb.Writer()} // LLM nil
	spec := Spec{Code: `function handler(req, ctx){ return { s: ctx.llm("x") }; }`}
	if _, err := Execute(rt, spec, map[string]any{}); err == nil {
		t.Fatal("expected ctx.llm to fail when no provider is configured")
	}
}

// ctx.llm answers from the replay stand-in while a user-flow replay of the project runs,
// without resolving a provider; outside a replay it needs a configured one.
func TestNewLLMUsesTheStandInDuringAReplay(t *testing.T) {
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	const project = 9302
	call := NewLLM(project, nil, actionexec.CompleteDeps{GlobalDB: mem})

	if _, err := call("hello", "", 0); err == nil {
		t.Fatal("outside a replay there is no provider to answer, so the call must fail")
	}
	end := replayoutbox.Begin(project)
	defer end()
	got, err := call("hello", "", 0)
	if err != nil || got != replaymodel.Text {
		t.Fatalf("during a replay the stand-in answers, got %q err=%v", got, err)
	}
}
