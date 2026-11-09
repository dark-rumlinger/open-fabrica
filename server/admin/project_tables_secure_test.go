package admin

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	sdeps "openfabrica/server/deps"

	_ "modernc.org/sqlite"
)

// The admin row editor hashes/encrypts values bound for PASSWORD/ENCRYPTED
// columns on the way in. That processing must fail CLOSED: when the
// secure-column projection can't be read, the handler cannot tell which columns
// are secret, and skipping the step writes the caller's value into a PASSWORD
// column verbatim - a credential silently stored in plaintext, which then
// authenticates nobody and reads as a working row.
//
// LoadTableSchema runs first and reads the same registry table, so an outage
// that takes out the whole registry 404s upstream and never reaches this. The
// fault injected here is narrower and is the one that actually reaches it:
// schema_def stays readable while secure_columns does not.
func TestAdminRowInsertFailsClosedWhenSecureColumnsUnreadable(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(9)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`CREATE TABLE members (id INTEGER PRIMARY KEY, password TEXT)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES (?, ?, ?)`,
		"members",
		`{"id":"INTEGER","password":"PASSWORD"}`,
		`{"password":"hash"}`); err != nil {
		t.Fatalf("register table: %v", err)
	}
	// Registry skew: the column carrying the secure projection is gone, while
	// schema_def still reads. LoadTableSchema succeeds, LoadSecureColumns errors.
	if _, err := pdb.ExecWrite(`ALTER TABLE of_dynamic_tables DROP COLUMN secure_columns`); err != nil {
		t.Fatalf("drop projection column: %v", err)
	}

	h := &ProjectTablesHandler{deps: &Deps{Core: sdeps.Core{ProjectDBManager: mgr, Logger: slog.Default()}}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"data":{"password":"hunter2"}}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", "9")
	rctx.URLParams.Add("tableName", "members")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.InsertRow(w, req)

	if w.Code >= 200 && w.Code < 300 {
		t.Errorf("insert succeeded despite an unreadable secure-column projection: %d %s", w.Code, w.Body.String())
	}
	var plaintext int
	if err := pdb.QueryRow(
		`SELECT COUNT(*) FROM members WHERE password = 'hunter2'`).Scan(&plaintext); err != nil {
		t.Fatalf("count: %v", err)
	}
	if plaintext != 0 {
		t.Fatalf("password was stored in plaintext: the hash step was skipped because the projection could not be read")
	}
}

// Same contract on the update path.
func TestAdminRowUpdateFailsClosedWhenSecureColumnsUnreadable(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(9)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`CREATE TABLE members (id INTEGER PRIMARY KEY, password TEXT);
		 INSERT INTO members (id, password) VALUES (1, 'existing-hash');`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES (?, ?, ?)`,
		"members",
		`{"id":"INTEGER","password":"PASSWORD"}`,
		`{"password":"hash"}`); err != nil {
		t.Fatalf("register table: %v", err)
	}
	if _, err := pdb.ExecWrite(`ALTER TABLE of_dynamic_tables DROP COLUMN secure_columns`); err != nil {
		t.Fatalf("drop projection column: %v", err)
	}

	h := &ProjectTablesHandler{deps: &Deps{Core: sdeps.Core{ProjectDBManager: mgr, Logger: slog.Default()}}}
	req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"data":{"password":"hunter2"}}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", "9")
	rctx.URLParams.Add("tableName", "members")
	rctx.URLParams.Add("rowID", "1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.UpdateRow(w, req)

	if w.Code >= 200 && w.Code < 300 {
		t.Errorf("update succeeded despite an unreadable secure-column projection: %d %s", w.Code, w.Body.String())
	}
	var plaintext int
	if err := pdb.QueryRow(
		`SELECT COUNT(*) FROM members WHERE password = 'hunter2'`).Scan(&plaintext); err != nil {
		t.Fatalf("count: %v", err)
	}
	if plaintext != 0 {
		t.Fatalf("password was stored in plaintext: the hash step was skipped because the projection could not be read")
	}
}
