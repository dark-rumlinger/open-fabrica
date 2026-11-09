package admin

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/internal/tokenrevoke"
	sdeps "openfabrica/server/deps"
)

// Editing or deleting an app user's account in the admin's table view ends that
// user's sessions, exactly as the same change through the app's own API does;
// editing a harmless column, or any row of an ordinary table, does not.
func TestAdminRowEditsEndAppUserSessions(t *testing.T) {
	const projectID = 9
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	t.Cleanup(func() { tokenrevoke.Forget(projectID) })
	pdb, err := mgr.Create(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE members (id INTEGER PRIMARY KEY, email TEXT, password TEXT, role TEXT, display_name TEXT)`,
		`INSERT INTO members (email, password, role, display_name) VALUES ('ada@x.io','h','user','Ada'), ('bob@x.io','h','user','Bob')`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('members','{"id":"INTEGER","email":"TEXT","password":"PASSWORD","role":"TEXT","display_name":"TEXT"}','{"password":"hash"}')`,
		`INSERT INTO of_auth_endpoints (table_name, path) VALUES ('members','auth')`,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, role TEXT)`,
		`INSERT INTO notes (role) VALUES ('x')`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('notes','{"id":"INTEGER","role":"TEXT"}','{}')`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	h := &ProjectTablesHandler{deps: &Deps{Core: sdeps.Core{ProjectDBManager: mgr, Logger: slog.Default()}}}

	call := func(handler func(http.ResponseWriter, *http.Request), method, table, row, body string) int {
		req := httptest.NewRequest(method, "/", strings.NewReader(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("projectID", "9")
		rctx.URLParams.Add("tableName", table)
		rctx.URLParams.Add("rowID", row)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	revoked := func(userID int, issuedAt time.Time) bool {
		return tokenrevoke.Revoked(pdb.Reader(), projectID, userID, &issuedAt)
	}

	issued := time.Now()
	time.Sleep(3 * time.Millisecond)

	if code := call(h.UpdateRow, "PUT", "members", "1", `{"data":{"display_name":"Ada L."}}`); code != 200 {
		t.Fatalf("display-name edit: status %d", code)
	}
	if revoked(1, issued) {
		t.Error("editing a display name ended the user's session")
	}
	if code := call(h.UpdateRow, "PUT", "notes", "1", `{"data":{"role":"admin"}}`); code != 200 {
		t.Fatalf("ordinary-table edit: status %d", code)
	}
	if revoked(1, issued) {
		t.Error("editing an ordinary table ended a session")
	}

	if code := call(h.UpdateRow, "PUT", "members", "1", `{"data":{"role":"admin"}}`); code != 200 {
		t.Fatalf("role edit: status %d", code)
	}
	if !revoked(1, issued) {
		t.Error("changing a user's role did not end their session")
	}
	if revoked(2, issued) {
		t.Error("changing one user's role ended another's session")
	}

	if code := call(h.DeleteRow, "DELETE", "members", "2", ``); code != 200 {
		t.Fatalf("delete: status %d", code)
	}
	if !revoked(2, issued) {
		t.Error("deleting a user did not end their session")
	}
}
