package admin

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	sdeps "openfabrica/server/deps"

	_ "modernc.org/sqlite"
)

// TestProjectFilesGuardPlatformManaged pins that the owner-facing admin API
// refuses to delete OR overwrite a platform-managed file (js/auth.js,
// js/e2ee.js, or anything flagged metadata.platform_managed), while ordinary
// files delete/edit normally. This is the human-facing backstop to the
// model-facing tool guard.
func TestProjectFilesGuardPlatformManaged(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(7)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_files (kind, path, body, scope, storage, content_type, metadata, status, is_deleted)
		 VALUES
		 ('file','js/e2ee.js','export const session=1;','global','inline','application/javascript','{"platform_managed":true,"is_module":true}','published',0),
		 ('file','js/widget.js','console.log(1)','page','inline','application/javascript','{}','published',0)`,
	); err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}
	idOf := func(path string) int {
		var id int
		if err := pdb.QueryRow("SELECT id FROM of_files WHERE path = ?", path).Scan(&id); err != nil {
			t.Fatalf("id for %s: %v", path, err)
		}
		return id
	}
	protID, normID := idOf("js/e2ee.js"), idOf("js/widget.js")

	h := &ProjectFilesHandler{deps: &Deps{Core: sdeps.Core{ProjectDBManager: mgr, Logger: slog.Default()}}}

	call := func(method string, fileID int, fn http.HandlerFunc, body string) *httptest.ResponseRecorder {
		var rdr *strings.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		} else {
			rdr = strings.NewReader("")
		}
		req := httptest.NewRequest(method, "/", rdr)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("projectID", "7")
		rctx.URLParams.Add("fileID", strconv.Itoa(fileID))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		fn(w, req)
		return w
	}
	isDeleted := func(id int) int {
		var d int
		pdb.QueryRow("SELECT is_deleted FROM of_files WHERE id = ?", id).Scan(&d)
		return d
	}
	bodyOf := func(id int) string {
		var b string
		pdb.QueryRow("SELECT body FROM of_files WHERE id = ?", id).Scan(&b)
		return b
	}

	// Delete: protected refused (403, still present); ordinary deletes (200).
	if w := call("DELETE", protID, h.Delete, ""); w.Code != http.StatusForbidden {
		t.Fatalf("delete protected: expected 403, got %d (%s)", w.Code, w.Body.String())
	}
	if isDeleted(protID) != 0 {
		t.Fatal("protected file must remain after refused delete")
	}
	if w := call("DELETE", normID, h.Delete, ""); w.Code != http.StatusOK {
		t.Fatalf("delete ordinary: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if isDeleted(normID) != 1 {
		t.Fatal("ordinary file should be soft-deleted")
	}

	// UpdateContent: protected overwrite refused (403, body unchanged).
	if w := call("PUT", protID, h.UpdateContent, `{"content":"hacked"}`); w.Code != http.StatusForbidden {
		t.Fatalf("edit protected: expected 403, got %d (%s)", w.Code, w.Body.String())
	}
	if got := bodyOf(protID); got != "export const session=1;" {
		t.Fatalf("protected file body must be unchanged, got %q", got)
	}
}
