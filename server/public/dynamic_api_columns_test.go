package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/db/models"
	"openfabrica/security"
)

func columnsTestDB(t *testing.T, ddl ...string) *sql.DB {
	t.Helper()
	db := setupRowIDColumnDB(t, ddl...)
	db.SetMaxOpenConns(1)
	return db
}

func listRows(t *testing.T, h *Handler, db *sql.DB, ep *apiEndpoint, query string) (int, []map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/items?"+query, nil)
	rec := httptest.NewRecorder()
	h.apiList(rec, req, db, "items", ep, &apiRequestContext{})
	var env struct {
		Rows []map[string]any `json:"rows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env.Rows
}

const itemsDDL = `CREATE TABLE items (id INTEGER PRIMARY KEY, title TEXT, secret TEXT, status TEXT DEFAULT 'open', paid INTEGER DEFAULT 0)`
const itemsSeed = `INSERT INTO items (id, title, secret) VALUES (1, 'alpha', 'hunter2'), (2, 'beta', 'swordfish')`

func TestApiListHiddenColumnsCannotBeProbed(t *testing.T) {
	db := columnsTestDB(t, itemsDDL, itemsSeed)
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{PublicColumns: []string{"id", "title"}, SecureCols: map[string]string{}}

	for name, query := range map[string]string{
		"exact filter": "secret=hunter2",
		"like filter":  "secret__like=hunt",
		"sort":         "sort=secret",
	} {
		code, _ := listRows(t, h, db, ep, query)
		if code != http.StatusBadRequest {
			t.Errorf("%s on a hidden column: status %d, want 400", name, code)
		}
	}

	// Free-text search must not match text that only lives in a hidden column.
	code, rows := listRows(t, h, db, ep, "q=swordfish")
	if code != http.StatusOK || len(rows) != 0 {
		t.Errorf("q matched a hidden column: status=%d rows=%v", code, rows)
	}
	// Exposed columns still filter, search and sort.
	if code, rows := listRows(t, h, db, ep, "title__like=alp"); code != http.StatusOK || len(rows) != 1 {
		t.Errorf("exposed filter: status=%d rows=%v", code, rows)
	}
	if code, rows := listRows(t, h, db, ep, "q=beta&sort=title"); code != http.StatusOK || len(rows) != 1 {
		t.Errorf("exposed search: status=%d rows=%v", code, rows)
	}
}

func TestApiListWithoutPublicColumnsKeepsFiltering(t *testing.T) {
	db := columnsTestDB(t, itemsDDL, itemsSeed)
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{SecureCols: map[string]string{}}
	if code, rows := listRows(t, h, db, ep, "secret=hunter2"); code != http.StatusOK || len(rows) != 1 {
		t.Errorf("no public_columns must not restrict filters: status=%d rows=%v", code, rows)
	}
}

// TestApiEncryptedColumnsHidden pins that ENCRYPTED columns are hidden from the
// visitor API exactly like PASSWORD columns. An ENCRYPTED column holds a
// server-side secret (ciphertext at rest); the trigger-events contract and every
// other read path (agent reads, the tools path) strip it, so the public CRUD API
// must too - never returning its ciphertext, even on the bare SELECT * path or
// when an author lists it in public_columns.
func TestApiEncryptedColumnsHidden(t *testing.T) {
	db := columnsTestDB(t, itemsDDL, itemsSeed)
	h := &Handler{deps: &Deps{}}

	// No public_columns -> the list query is SELECT *; the strip must drop the column.
	ep := &apiEndpoint{SecureCols: map[string]string{"secret": "encrypt"}}
	code, rows := listRows(t, h, db, ep, "")
	if code != http.StatusOK || len(rows) == 0 {
		t.Fatalf("SELECT * list: status=%d rows=%v", code, rows)
	}
	for _, row := range rows {
		if _, present := row["secret"]; present {
			t.Error("ENCRYPTED column 'secret' leaked in the SELECT * response (ciphertext exposure)")
		}
		if _, present := row["title"]; !present {
			t.Error("a normal column was dropped")
		}
	}

	// Listing the ENCRYPTED column in public_columns must not expose it either.
	epPub := &apiEndpoint{PublicColumns: []string{"id", "title", "secret"}, SecureCols: map[string]string{"secret": "encrypt"}}
	if code, rows := listRows(t, h, db, epPub, ""); code != http.StatusOK {
		t.Fatalf("public_columns list: status=%d", code)
	} else {
		for _, row := range rows {
			if _, present := row["secret"]; present {
				t.Error("ENCRYPTED column listed in public_columns was still returned")
			}
		}
	}

	// And it cannot be filtered, sorted, or aggregated on (columnReadable gates those).
	if columnReadable(ep, "secret") {
		t.Error("columnReadable allowed an ENCRYPTED column to be probed via filter/sort/aggregate")
	}
}

func TestApiStatsHiddenColumnsRejected(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, itemsDDL)
	mustExec(t, w, itemsSeed)
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, public_columns) VALUES ('items','items','["GET"]','["id","title"]')`)
	h := &Handler{deps: &Deps{
		ProjectDBManager: mgr,
		JWTManager:       security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour),
	}}

	status := func(query string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/items/_stats?"+query, nil)
		req = req.WithContext(context.WithValue(req.Context(), projectContextKey, &models.Project{ID: 1}))
		rec := httptest.NewRecorder()
		h.apiStats(rec, req, pdb.Writer(), "items")
		return rec.Code
	}
	for name, query := range map[string]string{
		"group_by": "fn=count&group_by=secret",
		"column":   "fn=max&column=secret",
		"filter":   "fn=count&secret=hunter2",
	} {
		if code := status(query); code != http.StatusBadRequest {
			t.Errorf("stats %s on a hidden column: status %d, want 400", name, code)
		}
	}
	if code := status("fn=count&group_by=title"); code != http.StatusOK {
		t.Errorf("stats on an exposed column: status %d, want 200", code)
	}
}

func writeItem(t *testing.T, h *Handler, db *sql.DB, ep *apiEndpoint, method, body string) int {
	t.Helper()
	req := httptest.NewRequest(method, "/api/items", strings.NewReader(body))
	rec := httptest.NewRecorder()
	rc := &apiRequestContext{}
	if method == http.MethodPost {
		h.apiInsert(rec, req, db, "items", ep, 1, "items", rc)
	} else {
		h.apiUpdate(rec, req, db, "items", "1", ep, 1, "items", rc)
	}
	return rec.Code
}

func itemState(t *testing.T, db *sql.DB, id int) (title, status string, paid int) {
	t.Helper()
	if err := db.QueryRow(`SELECT title, status, paid FROM items WHERE id = ?`, id).Scan(&title, &status, &paid); err != nil {
		t.Fatal(err)
	}
	return
}

func TestWritableColumnsDropPrivilegedFields(t *testing.T) {
	db := columnsTestDB(t, itemsDDL, itemsSeed)
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{WritableColumns: []string{"title"}, SecureCols: map[string]string{}}

	if code := writeItem(t, h, db, ep, http.MethodPost, `{"title":"gamma","status":"approved","paid":1}`); code != http.StatusCreated {
		t.Fatalf("insert status %d", code)
	}
	title, status, paid := itemState(t, db, 3)
	if title != "gamma" || status != "open" || paid != 0 {
		t.Fatalf("insert must keep privileged fields at defaults, got %q %q %d", title, status, paid)
	}

	if code := writeItem(t, h, db, ep, http.MethodPut, `{"title":"renamed","status":"approved","paid":1}`); code != http.StatusOK {
		t.Fatalf("update status %d", code)
	}
	title, status, paid = itemState(t, db, 1)
	if title != "renamed" || status != "open" || paid != 0 {
		t.Fatalf("update must ignore privileged fields, got %q %q %d", title, status, paid)
	}
}

func TestWritableColumnsUnsetKeepsOpenWrites(t *testing.T) {
	db := columnsTestDB(t, itemsDDL, itemsSeed)
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{SecureCols: map[string]string{}}
	if code := writeItem(t, h, db, ep, http.MethodPut, `{"status":"approved"}`); code != http.StatusOK {
		t.Fatalf("update status %d", code)
	}
	if _, status, _ := itemState(t, db, 1); status != "approved" {
		t.Fatalf("without writable_columns every column stays writable, got %q", status)
	}
}

func TestLoadEndpointReadsWritableColumns(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	mustExec(t, pdb.Writer(), `INSERT INTO of_api_endpoints (path, table_name, methods, writable_columns) VALUES ('notes','notes','["POST"]','["body"]')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	ep, err := h.loadEndpoint(pdb.Reader(), "notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.WritableColumns) != 1 || ep.WritableColumns[0] != "body" {
		t.Fatalf("WritableColumns = %v", ep.WritableColumns)
	}
}

func TestFilterSignupBody(t *testing.T) {
	ae := &authEndpointConfig{UsernameColumn: "email"}
	body := map[string]any{"email": "a@b.c", "name": "A", "is_admin": true, "Credits": 99, "plan": "pro"}
	filterSignupBody(body, ae)
	if _, ok := body["is_admin"]; ok {
		t.Error("default filter must drop is_admin")
	}
	if _, ok := body["Credits"]; ok {
		t.Error("default filter must drop credits regardless of case")
	}
	if body["email"] == nil || body["name"] == nil {
		t.Errorf("default filter must keep ordinary fields: %v", body)
	}

	ae.SignupColumns = []string{"name"}
	body = map[string]any{"email": "a@b.c", "name": "A", "bio": "x", "avatar": "y"}
	filterSignupBody(body, ae)
	if len(body) != 2 || body["email"] == nil || body["name"] == nil {
		t.Errorf("signup_columns must keep only username and listed columns: %v", body)
	}
}

func TestRegistrationFailureHidesSQL(t *testing.T) {
	cases := map[string]int{
		"UNIQUE constraint failed: users.email":  http.StatusConflict,
		"NOT NULL constraint failed: users.name": http.StatusBadRequest,
		"table users has no column named secret": http.StatusBadRequest,
		"database is locked (5) near \"INSERT\"": http.StatusBadRequest,
	}
	for msg, want := range cases {
		code, out := registrationFailure(errString(msg))
		if code != want {
			t.Errorf("%q: status %d, want %d", msg, code, want)
		}
		if strings.Contains(out, "INSERT") || strings.Contains(out, "users.") || strings.Contains(out, "constraint") {
			t.Errorf("%q leaked SQL detail: %q", msg, out)
		}
	}
	if _, out := registrationFailure(errString("NOT NULL constraint failed: users.name")); !strings.Contains(out, "name") {
		t.Errorf("a missing field should be named, got %q", out)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
