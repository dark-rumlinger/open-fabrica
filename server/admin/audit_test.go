package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
	"openfabrica/server/middleware"

	_ "modernc.org/sqlite"
)

func newAuditHandler(t *testing.T) (*AuditHandler, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatalf("open core db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return &AuditHandler{deps: &Deps{Core: sdeps.Core{DB: d, Logger: slog.Default()}}}, d
}

func auditRowCount(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	if err := d.DB.QueryRow(`SELECT COUNT(*) FROM admin_audit`).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// A state-changing request is recorded with the acting user, target project,
// response status, and the request id from the X-Request-ID chain.
func TestAuditRecordsMutation(t *testing.T) {
	h, d := newAuditHandler(t)

	chain := middleware.RequestID(h.record(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	req := httptest.NewRequest("DELETE", "/admin/api/projects/7/pages/3", nil)
	req.Header.Set("X-Request-ID", "req-xyz")
	ctx := context.WithValue(req.Context(), security.ClaimsKey, &security.Claims{UserID: 42, Username: "alice", Role: "admin"})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", "7")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	chain.ServeHTTP(httptest.NewRecorder(), req)

	var method, username, reqID string
	var userID, projectID, status int
	err := d.DB.QueryRow(
		`SELECT method, username, user_id, target_project_id, status, request_id FROM admin_audit ORDER BY id DESC LIMIT 1`,
	).Scan(&method, &username, &userID, &projectID, &status, &reqID)
	if err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if method != "DELETE" || username != "alice" || userID != 42 || projectID != 7 || status != http.StatusNoContent || reqID != "req-xyz" {
		t.Errorf("audit row mismatch: method=%q user=%q(%d) project=%d status=%d reqID=%q",
			method, username, userID, projectID, status, reqID)
	}
}

// Reads are not audited: a GET must leave the trail empty.
func TestAuditSkipsReads(t *testing.T) {
	h, d := newAuditHandler(t)
	chain := h.record(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	chain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/admin/api/projects/7/pages", nil))
	if n := auditRowCount(t, d); n != 0 {
		t.Errorf("GET should not be audited, found %d rows", n)
	}
}

// List returns newest-first with the {items,total} envelope and honors the
// method filter.
func TestAuditListFilters(t *testing.T) {
	h, d := newAuditHandler(t)
	seed := []struct {
		method, user string
	}{{"POST", "alice"}, {"DELETE", "bob"}, {"PUT", "alice"}}
	for _, s := range seed {
		if _, err := d.ExecWrite(
			`INSERT INTO admin_audit (user_id, username, method, path, status) VALUES (1, ?, ?, '/x', 200)`,
			s.user, s.method); err != nil {
			t.Fatalf("seed audit: %v", err)
		}
	}

	req := httptest.NewRequest("GET", "/?action=delete", nil)
	w := httptest.NewRecorder()
	h.List(w, req)
	if w.Code != 200 {
		t.Fatalf("List status %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("action=delete should match 1 row, got total=%d items=%d", out.Total, len(out.Items))
	}
	if out.Items[0]["method"] != "DELETE" || out.Items[0]["username"] != "bob" {
		t.Errorf("unexpected row: %v", out.Items[0])
	}
}
