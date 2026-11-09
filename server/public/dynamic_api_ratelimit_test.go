package public

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"

	_ "modernc.org/sqlite"
)

// TestDynamicAPIEnforcesConfiguredRateLimit proves the per-endpoint rate_limit
// configured via manage_api is enforced on the generic CRUD path. With
// rate_limit=1 the first GET is admitted and a second within the same minute is
// rejected with 429. Scanning the value into the endpoint config is not enough:
// it must reach the limiter.
func TestDynamicAPIEnforcesConfiguredRateLimit(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	w := pdb.Writer()
	if _, err := w.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		VALUES ('items', '{"name":"TEXT"}', '{}')`); err != nil {
		t.Fatalf("register items table: %v", err)
	}
	if _, err := w.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`); err != nil {
		t.Fatalf("create items table: %v", err)
	}
	if _, err := w.Exec(`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, rate_limit)
		VALUES ('rltest', 'items', '["GET"]', 0, 1)`); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	get := func() int {
		req := httptest.NewRequest("GET", "/api/rltest", nil)
		req = req.WithContext(context.WithValue(req.Context(), projectContextKey, &models.Project{ID: 1}))
		rec := httptest.NewRecorder()
		h.DynamicAPI(rec, req)
		return rec.Code
	}

	// First request is within the configured budget and consumes the single
	// per-minute token.
	if code := get(); code == http.StatusTooManyRequests {
		t.Fatalf("first request within budget returned 429; the configured limit should admit it")
	}

	// Second request within the same minute trips the configured rate_limit=1.
	if code := get(); code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429 (configured rate_limit=1 was not enforced)", code)
	}
}

// TestDynamicAPIRateLimitZeroDisabled pins that rate_limit=0 disables the cap
// (the escape hatch for a deliberately unthrottled public endpoint): repeated
// requests are never 429'd on rate-limit grounds.
func TestDynamicAPIRateLimitZeroDisabled(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	w := pdb.Writer()
	if _, err := w.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns)
		VALUES ('items', '{"name":"TEXT"}', '{}')`); err != nil {
		t.Fatalf("register items table: %v", err)
	}
	if _, err := w.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`); err != nil {
		t.Fatalf("create items table: %v", err)
	}
	if _, err := w.Exec(`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, rate_limit)
		VALUES ('nolimit', 'items', '["GET"]', 0, 0)`); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/api/nolimit", nil)
		req = req.WithContext(context.WithValue(req.Context(), projectContextKey, &models.Project{ID: 1}))
		rec := httptest.NewRecorder()
		h.DynamicAPI(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d returned 429 though rate_limit=0 disables the cap", i+1)
		}
	}
}
