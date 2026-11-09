package admin

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"

	"openfabrica/config"
	"openfabrica/db"
	sdeps "openfabrica/server/deps"
	"openfabrica/tlsproxy"
)

const projectsTestSchema = `
CREATE TABLE projects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    domain TEXT UNIQUE,
    description TEXT,
    llm_model_id INTEGER,
    published INTEGER NOT NULL DEFAULT 1,
    mode TEXT NOT NULL DEFAULT 'building',
    config TEXT NOT NULL DEFAULT '{}',
    source_project_id INTEGER,
    ambition_level TEXT NOT NULL DEFAULT 'high',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

// newHostingTestRouter builds a router with the hosting routes over an
// in-memory DB seeded with one project per given domain (id = 1-based index;
// empty string means no domain).
func newHostingTestRouter(t *testing.T, cfg *config.Config, domains ...string) chi.Router {
	t.Helper()
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	if _, err := mem.Exec(projectsTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for i, d := range domains {
		var domain *string
		if d != "" {
			domain = &d
		}
		if _, err := mem.Exec("INSERT INTO projects (id, name, domain, llm_model_id) VALUES (?, ?, ?, 1)", i+1, "p", domain); err != nil {
			t.Fatalf("seed project: %v", err)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := &Deps{Core: sdeps.Core{
		Config:     cfg,
		DB:         &db.DB{DB: mem},
		Logger:     logger,
		TLSManager: tlsproxy.NewManager(cfg, mem, logger),
	}}
	h := &HostingHandler{deps: deps}

	r := chi.NewRouter()
	r.Get("/projects/{projectID}/hosting/status", h.Status)
	r.Get("/projects/{projectID}/hosting/qr", h.QR)
	return r
}

func getJSON(t *testing.T, r chi.Router, url string, wantCode int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != wantCode {
		t.Fatalf("GET %s: got %d, want %d (body %s)", url, w.Code, wantCode, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: bad JSON: %v", url, err)
	}
	return out
}

// TestHostingStatusClassification pins the payload the status chip and Go Live
// wizard key off of: domain kind, HTTPS applicability, and the pending default
// for a public domain that has not been probed.
func TestHostingStatusClassification(t *testing.T) {
	cfg := &config.Config{PublicPort: 5733, DataDir: t.TempDir()}
	r := newHostingTestRouter(t, cfg, "app.localhost", "example.com", "")

	local := getJSON(t, r, "/projects/1/hosting/status", http.StatusOK)
	if local["domain_kind"] != "local" {
		t.Errorf("local domain_kind = %v, want local", local["domain_kind"])
	}
	if https := local["https"].(map[string]any); https["state"] != "not_applicable" {
		t.Errorf("local https.state = %v, want not_applicable", https["state"])
	}

	pub := getJSON(t, r, "/projects/2/hosting/status", http.StatusOK)
	if pub["domain_kind"] != "public" {
		t.Errorf("public domain_kind = %v, want public", pub["domain_kind"])
	}
	if https := pub["https"].(map[string]any); https["state"] != "pending" {
		t.Errorf("public https.state = %v, want pending (never probed)", https["state"])
	}
	if pub["public_port"] != float64(5733) {
		t.Errorf("public_port = %v, want 5733", pub["public_port"])
	}

	none := getJSON(t, r, "/projects/3/hosting/status", http.StatusOK)
	if none["domain_kind"] != "none" {
		t.Errorf("no-domain domain_kind = %v, want none", none["domain_kind"])
	}
}

// TestHostingStatusTLSDisabled verifies the disabled state wins over domain
// classification for the HTTPS slice.
func TestHostingStatusTLSDisabled(t *testing.T) {
	cfg := &config.Config{PublicPort: 5733, DataDir: t.TempDir(), TLSDisabled: true}
	r := newHostingTestRouter(t, cfg, "example.com")

	st := getJSON(t, r, "/projects/1/hosting/status", http.StatusOK)
	if st["tls_disabled"] != true {
		t.Errorf("tls_disabled = %v, want true", st["tls_disabled"])
	}
	if https := st["https"].(map[string]any); https["state"] != "disabled" {
		t.Errorf("https.state = %v, want disabled", https["state"])
	}
}

// TestHostingQR verifies the phone-test payload: an https URL for a public
// domain, an SVG data URI, and a 400 when no domain is configured.
func TestHostingQR(t *testing.T) {
	cfg := &config.Config{PublicPort: 5733, DataDir: t.TempDir()}
	r := newHostingTestRouter(t, cfg, "example.com", "")

	qr := getJSON(t, r, "/projects/1/hosting/qr", http.StatusOK)
	if qr["url"] != "https://example.com" {
		t.Errorf("qr url = %v, want https://example.com", qr["url"])
	}
	if s, _ := qr["qr"].(string); !strings.HasPrefix(s, "data:image/svg+xml;base64,") {
		t.Errorf("qr payload is not an SVG data URI (got %.40v...)", qr["qr"])
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/2/hosting/qr", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("qr without domain: got %d, want 400", w.Code)
	}
}

// TestPublicURLFor pins the URL scheme choice mirrored by the frontend's
// formatPublicUrl: https for real domains, http with the public port for
// local/IP domains, http when TLS is off.
func TestPublicURLFor(t *testing.T) {
	cases := []struct {
		domain      string
		tlsDisabled bool
		want        string
	}{
		{"example.com", false, "https://example.com"},
		{"example.com", true, "http://example.com:5733"},
		{"app.localhost", false, "http://app.localhost:5733"},
		{"192.168.1.10", false, "http://192.168.1.10:5733"},
	}
	for _, c := range cases {
		if got := publicURLFor(c.domain, 5733, c.tlsDisabled); got != c.want {
			t.Errorf("publicURLFor(%q, tlsDisabled=%v) = %q, want %q", c.domain, c.tlsDisabled, got, c.want)
		}
	}
}
