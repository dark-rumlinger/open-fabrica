package public

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A PASSWORD column is stored as TEXT, so the export's PRAGMA type check never saw
// it and password hashes streamed out in the CSV. The registered secure-column
// metadata must strip it - from the derived column list AND from an explicit
// public_columns list.
func TestCSVExportNeverStreamsPasswordHashes(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `CREATE TABLE members (id INTEGER PRIMARY KEY, email TEXT, password TEXT)`)
	mustExec(t, w, `INSERT INTO members (email, password) VALUES ('ada@example.com', '$2a$10$secrethashvalue')`)
	mustExec(t, w, `INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('members','{}','{"password":"hash"}')`)
	mustExec(t, w, `INSERT INTO of_export_endpoints (path, table_name) VALUES ('members-all','members')`)
	mustExec(t, w, `INSERT INTO of_export_endpoints (path, table_name, public_columns) VALUES ('members-explicit','members','["email","password"]')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	for _, path := range []string{"members-all", "members-explicit"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/"+path+"/export", nil)
		h.apiCSVExport(rec, req, 1, pdb.Reader(), path)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %q", path, rec.Code, body)
		}
		if strings.Contains(body, "secrethashvalue") || strings.Contains(strings.SplitN(body, "\n", 2)[0], "password") {
			t.Errorf("%s: export leaked the PASSWORD column:\n%s", path, body)
		}
		if !strings.Contains(body, "ada@example.com") {
			t.Errorf("%s: export dropped a public column:\n%s", path, body)
		}
	}
}
