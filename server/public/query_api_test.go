package public

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/db/models"
)

// TestValidateReadOnlySelect pins the runtime query-endpoint guard: read-only
// verbs only, single statement, no system (of_) tables.
//
// The CTE rows are the point of the EffectiveVerb switch: this endpoint must accept
// exactly what the build-time manage_sql path accepts, or a query the model verified
// against the same data fails only once it is wired into an endpoint - a break no
// gate, validator, or page probe can see.
func TestValidateReadOnlySelect(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		wantError bool
	}{
		{"plain select", "SELECT * FROM orders", false},
		{"join", "SELECT o.id FROM orders o JOIN customers c ON o.customer_id = c.id", false},
		{"trailing semicolon ok", "SELECT 1;", false},
		{"lowercase", "select name from products", false},
		{"column named like of_", "SELECT number_of_items FROM carts", false},
		{"values", "VALUES (1), (2)", false},
		{"read-only cte", "WITH recent AS (SELECT * FROM orders WHERE total > 10) SELECT * FROM recent", false},
		{"recursive cte", "WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 5) SELECT n FROM seq", false},
		{"multi cte", "WITH a AS (SELECT 1 AS x), b AS (SELECT 2 AS y) SELECT x, y FROM a, b", false},
		{"empty", "", true},
		{"insert", "INSERT INTO orders (id) VALUES (1)", true},
		{"update", "UPDATE orders SET total = 0", true},
		{"delete", "DELETE FROM orders", true},
		{"drop", "DROP TABLE orders", true},
		{"pragma", "PRAGMA table_info(orders)", true},
		{"multi statement", "SELECT 1; DROP TABLE orders", true},
		{"system table", "SELECT * FROM of_secrets", true},
		{"system table qualified", "SELECT s.value FROM of_secrets s", true},
		{"cte-wrapped delete", "WITH doomed AS (SELECT id FROM orders) DELETE FROM orders WHERE id IN (SELECT id FROM doomed)", true},
		{"cte-wrapped insert", "WITH src AS (SELECT 1 AS v) INSERT INTO orders (total) SELECT v FROM src", true},
		{"cte reaching a system table", "WITH s AS (SELECT * FROM of_secrets) SELECT * FROM s", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errMsg := validateReadOnlySelect(tc.sql)
			if tc.wantError && errMsg == "" {
				t.Errorf("expected rejection for %q, got none", tc.sql)
			}
			if !tc.wantError && errMsg != "" {
				t.Errorf("expected %q to pass, got %q", tc.sql, errMsg)
			}
		})
	}
}

// TestApiListOverViewWithoutID is the regression for the "ORDER BY id" crash:
// a read-only endpoint over a view that has no id column must list rows, not
// 500 on a missing default sort column.
func TestApiListOverViewWithoutID(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, name TEXT, stock INTEGER)`,
		`INSERT INTO products (id, name, stock) VALUES (1,'a',2),(2,'b',50),(3,'c',1)`,
		`CREATE VIEW low_stock AS SELECT name, stock FROM products WHERE stock < 10`)

	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{TableName: "low_stock", ReadOnly: true}
	rc := &apiRequestContext{}

	req := httptest.NewRequest("GET", "/api/low_stock", nil)
	rec := httptest.NewRecorder()
	h.apiList(rec, req, db, "low_stock", ep, rc)

	if rec.Code != 200 {
		t.Fatalf("apiList over view status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c, _ := env["count"].(float64); int(c) != 2 {
		t.Errorf("expected 2 low-stock rows, got %v (%s)", env["count"], rec.Body.String())
	}
	rows, ok := env["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Errorf("LIST envelope must carry the array under `rows`, got %v (%s)", env["rows"], rec.Body.String())
	}
	if _, present := env["data"]; present {
		t.Errorf("LIST envelope must NOT expose a `data` field (hard cutover to `rows`): %s", rec.Body.String())
	}
}

// TestApiQueryRunsSelect drives the opt-in raw-SELECT endpoint end to end: a
// join returns the {columns, rows, row_count, execution_ms} envelope, and a
// write statement is rejected with 400.
func TestApiQueryRunsSelect(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE of_query_endpoints (id INTEGER PRIMARY KEY, path TEXT UNIQUE, max_rows INTEGER, requires_auth INTEGER, rate_limit INTEGER)`,
		`INSERT INTO of_query_endpoints (path, max_rows, requires_auth, rate_limit) VALUES ('_query', 200, 0, 60)`,
		`CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER, total REAL)`,
		`INSERT INTO customers (id,name) VALUES (1,'Ada'),(2,'Linus')`,
		`INSERT INTO orders (id,customer_id,total) VALUES (1,1,10.0),(2,1,5.0),(3,2,7.0)`)

	h := &Handler{deps: &Deps{}}
	project := &models.Project{ID: 1}

	// A real join, exactly the kind the build's fake client-side engine couldn't run.
	body := `{"sql":"SELECT c.name, o.total FROM orders o JOIN customers c ON o.customer_id = c.id ORDER BY o.id"}`
	req := httptest.NewRequest("POST", "/api/_query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiQuery(rec, req, project, db, "_query")

	if rec.Code != 200 {
		t.Fatalf("apiQuery status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Columns     []string `json:"columns"`
		Rows        [][]any  `json:"rows"`
		RowCount    int      `json:"row_count"`
		ExecutionMs float64  `json:"execution_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Columns) != 2 || out.Columns[0] != "name" || out.Columns[1] != "total" {
		t.Errorf("columns = %v, want [name total]", out.Columns)
	}
	if out.RowCount != 3 || len(out.Rows) != 3 {
		t.Errorf("row_count = %d, want 3 (%s)", out.RowCount, rec.Body.String())
	}

	// A write must be rejected with 400 and a clear error.
	wreq := httptest.NewRequest("POST", "/api/_query", strings.NewReader(`{"sql":"DELETE FROM orders"}`))
	wrec := httptest.NewRecorder()
	h.apiQuery(wrec, wreq, project, db, "_query")
	if wrec.Code != 400 {
		t.Fatalf("write query status = %d, want 400", wrec.Code)
	}
}

// TestApiQueryNotEnabled returns 404 when the project never opted in.
func TestApiQueryNotEnabled(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE of_query_endpoints (id INTEGER PRIMARY KEY, path TEXT UNIQUE, max_rows INTEGER, requires_auth INTEGER, rate_limit INTEGER)`)
	h := &Handler{deps: &Deps{}}
	req := httptest.NewRequest("POST", "/api/_query", strings.NewReader(`{"sql":"SELECT 1"}`))
	rec := httptest.NewRecorder()
	h.apiQuery(rec, req, &models.Project{ID: 1}, db, "_query")
	if rec.Code != 404 {
		t.Fatalf("not-enabled status = %d, want 404", rec.Code)
	}
}

// The public query endpoint never reads a table an auth endpoint authenticates
// against, whatever the SQL calls it.
func TestGuardQueryTargetsBlocksAuthTables(t *testing.T) {
	d := columnsTestDB(t,
		`CREATE TABLE of_auth_endpoints (table_name TEXT)`,
		`INSERT INTO of_auth_endpoints VALUES ('members')`,
	)
	if msg, _ := guardQueryTargets(d, "SELECT * FROM members"); !strings.Contains(msg, "protected table") {
		t.Errorf("auth table not blocked: %q", msg)
	}
	if msg, _ := guardQueryTargets(d, "SELECT 1"); msg != "" {
		t.Errorf("harmless query refused: %q", msg)
	}
}

// Hash columns are dropped from results by name, so a compound query that takes
// its column names from another branch, or a CTE that renames them, must be
// refused against a table holding a password hash; a plain SELECT, and queries
// over ordinary tables, still work. The memory-hungry blob functions are refused.
func TestGuardQueryTargetsHashTablesAndMemoryHogs(t *testing.T) {
	d := columnsTestDB(t,
		`CREATE TABLE of_auth_endpoints (table_name TEXT)`,
		`CREATE TABLE of_dynamic_tables (table_name TEXT, secure_columns TEXT)`,
		`INSERT INTO of_dynamic_tables VALUES ('vault', '{"pw":"hash"}'), ('notes', '')`,
	)
	for name, q := range map[string]string{
		"cte rename":     "WITH x(a, b) AS (SELECT * FROM vault) SELECT a, b FROM x",
		"union rename":   "SELECT 1, 2 UNION SELECT * FROM Vault",
		"values":         "SELECT * FROM vault WHERE id IN (VALUES (1))",
		"randomblob":     "SELECT hex(randomblob(500000000))",
		"zeroblob":       "SELECT length(zeroblob(900000000))",
		"load_extension": "SELECT load_extension('x')",
	} {
		if msg, _ := guardQueryTargets(d, q); msg == "" {
			t.Errorf("%s: query was allowed", name)
		}
	}
	for name, q := range map[string]string{
		"plain select on hash table": "SELECT * FROM vault",
		"count":                      "SELECT COUNT(*) FROM vault",
		"union over normal tables":   "SELECT 1 UNION SELECT id FROM notes",
		"cte over normal table":      "WITH x AS (SELECT * FROM notes) SELECT * FROM x",
	} {
		if msg, _ := guardQueryTargets(d, q); msg != "" {
			t.Errorf("%s: refused: %q", name, msg)
		}
	}
}
