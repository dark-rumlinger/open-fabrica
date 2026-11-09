package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/config"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
)

// buildTriageRouter wires just enough of the admin route tree to exercise the
// mounting decision for GET /admin/api/triage under a given admin bind address.
func buildTriageRouter(t *testing.T, adminBindAddr string) chi.Router {
	t.Helper()
	deps := &Deps{Core: sdeps.Core{
		Config:     &config.Config{AdminBindAddr: adminBindAddr},
		JWTManager: security.NewJWTManager("test-jwt-secret-at-least-32-bytes-long!!", time.Hour),
	}}
	r := chi.NewRouter()
	RegisterRoutes(r, deps, fstest.MapFS{})
	return r
}

// TestTriageLoopbackOnlyWhenNotExposed: with admin bound to loopback, the triage
// scan is mounted unauthenticated but fenced by the RemoteAddr loopback guard. A
// non-loopback peer must be forbidden (403) before any handler runs. The 403 (vs
// a 404) also proves the route is in fact mounted in this mode.
func TestTriageLoopbackOnlyWhenNotExposed(t *testing.T) {
	r := buildTriageRouter(t, "127.0.0.1")

	req := httptest.NewRequest(http.MethodGet, "/admin/api/triage", nil)
	req.RemoteAddr = "203.0.113.7:44321"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback triage: got %d, want 403", w.Code)
	}
}

// TestTriageRequiresAuthWhenExposed is the core regression for the reverse-proxy
// spoofing risk: when admin is exposed off loopback, a request that *looks* local
// (RemoteAddr 127.0.0.1, as a proxy would present) but carries no admin token
// must be rejected with 401 rather than served by the loopback path.
func TestTriageRequiresAuthWhenExposed(t *testing.T) {
	r := buildTriageRouter(t, "0.0.0.0")

	req := httptest.NewRequest(http.MethodGet, "/admin/api/triage", nil)
	req.RemoteAddr = "127.0.0.1:5555" // proxy-spoofed loopback
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("exposed triage without token: got %d, want 401 (JWT-gated, loopback path removed)", w.Code)
	}
}

// TestTriage_TextFormatRequiresNarrowing verifies the report refuses to render
// every project at once. The check must precede any DB access, so a handler with
// no deps still answers.
func TestTriage_TextFormatRequiresNarrowing(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"format=text", http.StatusBadRequest},
		{"format=text&all=1", http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin/api/triage?"+tc.query, nil)
		(&TriageHandler{}).Scan(rec, req)
		if rec.Code != tc.want {
			t.Errorf("GET ?%s = %d, want %d", tc.query, rec.Code, tc.want)
		}
	}
}
