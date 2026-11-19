package public

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func csvImportRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "rows.csv")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(body))
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/"+path+"/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// An imported PASSWORD value is stored hashed, like any other write.
func TestCSVImportHashesPasswordColumns(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `CREATE TABLE members (id INTEGER PRIMARY KEY, email TEXT, password TEXT)`)
	mustExec(t, w, `INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('members','{}','{"password":"hash"}')`)
	mustExec(t, w, `INSERT INTO of_import_endpoints (path, table_name, mode, requires_auth, max_rows) VALUES ('members','members','insert',0,100)`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	rec := httptest.NewRecorder()
	h.apiCSVImport(rec, csvImportRequest(t, "members", "email,password\nada@example.com,hunter2\n"), 1, pdb, "members")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}
	var stored string
	if err := pdb.Reader().QueryRow(`SELECT password FROM members`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "hunter2" || !strings.HasPrefix(stored, "$2") {
		t.Errorf("stored password = %q, want a bcrypt hash", stored)
	}
}

// An owner-scoped table cannot be imported into anonymously.
func TestCSVImportScopedTableNeedsCaller(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT, user_id INTEGER)`)
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, owner_column) VALUES ('notes','notes','["GET"]','user_id')`)
	mustExec(t, w, `INSERT INTO of_import_endpoints (path, table_name, mode, requires_auth, max_rows) VALUES ('notes','notes','insert',0,100)`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	rec := httptest.NewRecorder()
	h.apiCSVImport(rec, csvImportRequest(t, "notes", "body\nhello\n"), 1, pdb, "notes")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401; body %q", rec.Code, rec.Body.String())
	}
	var n int
	_ = pdb.Reader().QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rows imported anonymously into an owner-scoped table", n)
	}
}
