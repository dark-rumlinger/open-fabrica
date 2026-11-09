package public

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/db/models"
)

func queryGuardFixture(t *testing.T) *Handler {
	t.Helper()
	return &Handler{deps: &Deps{}}
}

const queryGuardSchema = `CREATE TABLE of_query_endpoints (id INTEGER PRIMARY KEY, path TEXT UNIQUE, max_rows INTEGER, requires_auth INTEGER, rate_limit INTEGER)`

func runQuery(t *testing.T, h *Handler, ddl []string, body string) (int, map[string]any) {
	t.Helper()
	db := setupRowIDColumnDB(t, append([]string{
		queryGuardSchema,
		`INSERT INTO of_query_endpoints (path, max_rows, requires_auth, rate_limit) VALUES ('_query', 3, 0, 60)`,
	}, ddl...)...)
	req := httptest.NewRequest("POST", "/api/_query", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiQuery(rec, req, &models.Project{ID: 1}, db, "_query")
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A LIMIT inside a comment or a string literal must not switch the row cap off.
func TestApiQueryRowCapSurvivesLimitInText(t *testing.T) {
	h := queryGuardFixture(t)
	ddl := []string{
		`CREATE TABLE nums (id INTEGER PRIMARY KEY, label TEXT)`,
		`INSERT INTO nums (label) VALUES ('a'),('b'),('c'),('d'),('e'),('f')`,
	}
	for name, sql := range map[string]string{
		"comment": `SELECT id FROM nums /* limit */`,
		"literal": `SELECT id, 'no limit here' AS note FROM nums`,
	} {
		body, _ := json.Marshal(map[string]string{"sql": sql})
		code, out := runQuery(t, h, ddl, string(body))
		if code != 200 {
			t.Fatalf("%s: status %d (%v)", name, code, out)
		}
		if n, _ := out["row_count"].(float64); int(n) != 3 {
			t.Errorf("%s: row_count %v, want the cap of 3", name, out["row_count"])
		}
		if out["truncated"] != true {
			t.Errorf("%s: truncated = %v, want true", name, out["truncated"])
		}
	}
}

func TestApiQueryBlocksProtectedTables(t *testing.T) {
	h := queryGuardFixture(t)
	ddl := []string{
		`CREATE TABLE of_auth_endpoints (id INTEGER PRIMARY KEY, table_name TEXT)`,
		`INSERT INTO of_auth_endpoints (table_name) VALUES ('members')`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY, email TEXT)`,
		`CREATE TABLE of_payments (id INTEGER PRIMARY KEY, amount INTEGER)`,
	}
	for name, sql := range map[string]string{
		"auth table":      `SELECT email FROM members`,
		"auth quoted":     `SELECT * FROM "members"`,
		"of_payments":     `SELECT * FROM of_payments`,
		"sqlite master":   `SELECT sql FROM sqlite_master`,
		"pragma table fn": `SELECT * FROM pragma_table_info('members')`,
	} {
		body, _ := json.Marshal(map[string]string{"sql": sql})
		if code, _ := runQuery(t, h, ddl, string(body)); code != 400 {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
	body, _ := json.Marshal(map[string]string{"sql": `SELECT 1 AS ok`})
	if code, _ := runQuery(t, h, ddl, string(body)); code != 200 {
		t.Errorf("harmless query: status %d, want 200", code)
	}
}

func TestApiQueryDropsHashColumns(t *testing.T) {
	h := queryGuardFixture(t)
	ddl := []string{
		`CREATE TABLE of_dynamic_tables (id INTEGER PRIMARY KEY, table_name TEXT, schema_def TEXT, secure_columns TEXT)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('staff','{}','{"pin":"hash"}')`,
		`CREATE TABLE staff (id INTEGER PRIMARY KEY, name TEXT, pin TEXT)`,
		`INSERT INTO staff (name, pin) VALUES ('Ada', '$2a$12$abc')`,
	}
	code, out := runQuery(t, h, ddl, `{"sql":"SELECT * FROM staff"}`)
	if code != 200 {
		t.Fatalf("status %d (%v)", code, out)
	}
	for _, c := range out["columns"].([]any) {
		if c == "pin" {
			t.Errorf("hash column leaked through SELECT *: %v", out["columns"])
		}
	}
	if rows := out["rows"].([]any); len(rows[0].([]any)) != 2 {
		t.Errorf("row width %d, want 2 (id, name)", len(rows[0].([]any)))
	}
	if code, _ := runQuery(t, h, ddl, `{"sql":"SELECT pin FROM staff"}`); code != 400 {
		t.Errorf("naming a hash column: status %d, want 400", code)
	}
}
