package public

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/db/models"
	"openfabrica/security"
)

func TestDocumentFilename(t *testing.T) {
	for _, c := range []struct {
		pattern string
		data    map[string]any
		want    string
	}{
		{"invoice-{{number}}", map[string]any{"number": float64(17)}, "invoice-17.pdf"},
		{"{{who}}", map[string]any{"who": "../../etc/passwd"}, "etc-passwd.pdf"},
		{"{{missing}}", map[string]any{}, "document.pdf"},
		{"report.pdf", nil, "report.pdf"},
		{"{{#each broken", map[string]any{}, "document.pdf"},
	} {
		if got := documentFilename(c.pattern, c.data); got != c.want {
			t.Errorf("documentFilename(%q) = %q, want %q", c.pattern, got, c.want)
		}
	}
}

func docRequest(h *Handler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/invoice/pdf", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req = req.WithContext(context.WithValue(req.Context(), projectContextKey, &models.Project{ID: 1}))
	rec := httptest.NewRecorder()
	mgr := h.deps.ProjectDBManager
	pdb, _ := mgr.Open(1)
	h.apiDocument(rec, req, &models.Project{ID: 1}, pdb, "invoice")
	return rec
}

func TestDocumentEndpointGuards(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	mustExec(t, pdb.Writer(), `INSERT INTO of_document_endpoints (path, template_html, requires_auth) VALUES ('invoice', '<p>{{name}}</p>', 1)`)
	jwt := security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr, JWTManager: jwt}}

	if rec := docRequest(h, "", `{"data":{"name":"x"}}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", rec.Code)
	}
	if rec := docRequest(h, "not-a-token", `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d, want 401", rec.Code)
	}
	tok, _ := jwt.Generate(security.TokenRequest{ProjectID: 1, UserID: 4, Username: "u", Role: "user"})
	if rec := docRequest(h, tok, `{not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: %d, want 400", rec.Code)
	}
	if rec := docRequest(h, tok, `{"data":{"name":"`+strings.Repeat("x", maxDocumentBodyBytes)+`"}}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", rec.Code)
	}

	// With a valid request the outcome depends on a browser being installed: either
	// a PDF, or a clear 503. Never a 500 from the template or a panic.
	rec := docRequest(h, tok, `{"data":{"name":"<b>Ada</b>"}}`)
	switch rec.Code {
	case http.StatusOK:
		if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" || !strings.HasPrefix(rec.Body.String(), "%PDF") {
			t.Errorf("200 without a PDF: %q %q", ct, rec.Body.String()[:min(20, rec.Body.Len())])
		}
	case http.StatusServiceUnavailable:
		t.Logf("no browser on this machine: %s", strings.TrimSpace(rec.Body.String()))
	default:
		t.Errorf("render outcome %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDocumentEndpointUnknownPath(t *testing.T) {
	mgr, _ := spineDispatchDB(t)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	if rec := docRequest(h, "", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d, want 404", rec.Code)
	}
}
