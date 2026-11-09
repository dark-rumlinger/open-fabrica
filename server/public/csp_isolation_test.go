package public

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"openfabrica/db"
)

// TestProjectResolver_DomainIsolation pins the cross-tenant isolation guarantee:
// a request is served ONLY to the project whose domain exactly matches the Host,
// and an unmatched Host is a 404 - never a fall-through to another tenant.
func TestProjectResolver_DomainIsolation(t *testing.T) {
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	if _, err := mem.Exec(`CREATE TABLE projects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		domain TEXT,
		description TEXT,
		llm_model_id INTEGER NOT NULL DEFAULT 0,
		published INTEGER NOT NULL,
		mode TEXT NOT NULL DEFAULT '',
		config TEXT NOT NULL DEFAULT '',
		source_project_id INTEGER,
		ambition_level TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := mem.Exec(
		`INSERT INTO projects (name, domain, published, created_at, updated_at)
		 VALUES ('A', 'app-a.test', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	h := &Handler{deps: &Deps{DB: &db.DB{DB: mem}}}
	var served bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	})
	do := func(host string) *httptest.ResponseRecorder {
		served = false
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ProjectResolver(next).ServeHTTP(rec, req)
		return rec
	}

	// Matching host -> resolves and serves the project.
	if rec := do("app-a.test"); rec.Code != http.StatusOK || !served {
		t.Errorf("matching host: code=%d served=%v, want 200/true", rec.Code, served)
	}
	// Unmatched host -> 404, and the downstream handler never runs (no leak to
	// another tenant's project).
	if rec := do("app-b.test"); rec.Code != http.StatusNotFound || served {
		t.Errorf("unmatched host: code=%d served=%v, want 404/false", rec.Code, served)
	}
}
