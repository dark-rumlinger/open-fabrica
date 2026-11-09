package public

import (
	"context"
	"net/http/httptest"
	"testing"

	"openfabrica/db/models"
)

// TestResolveCSP: a project's per-project CSP override wins; otherwise the
// instance default applies, including when no project is resolved.
func TestResolveCSP(t *testing.T) {
	h := &Handler{deps: &Deps{PublicCSP: "default-src 'self'"}}
	base := httptest.NewRequest("GET", "/", nil)

	// No project in context -> instance default.
	if got := h.resolveCSP(base); got != "default-src 'self'" {
		t.Errorf("no project: got %q, want instance default", got)
	}

	// Project present but no override -> instance default.
	noOverride := &models.Project{Config: `{"banned_ips":["1.2.3.4"]}`}
	r1 := base.WithContext(context.WithValue(base.Context(), projectContextKey, noOverride))
	if got := h.resolveCSP(r1); got != "default-src 'self'" {
		t.Errorf("project without override: got %q, want instance default", got)
	}

	// Project with override -> override wins.
	override := &models.Project{Config: `{"csp":"default-src 'none'; img-src 'self'"}`}
	r2 := base.WithContext(context.WithValue(base.Context(), projectContextKey, override))
	if got := h.resolveCSP(r2); got != "default-src 'none'; img-src 'self'" {
		t.Errorf("project with override: got %q, want override", got)
	}
}

// TestProjectCSPParsing pins the config-blob accessor used by resolveCSP.
func TestProjectCSPParsing(t *testing.T) {
	if got := models.ProjectCSP(nil); got != "" {
		t.Errorf("nil project: got %q, want empty", got)
	}
	if got := models.ProjectCSP(&models.Project{Config: ""}); got != "" {
		t.Errorf("empty config: got %q, want empty", got)
	}
	if got := models.ProjectCSP(&models.Project{Config: "not json"}); got != "" {
		t.Errorf("malformed config: got %q, want empty", got)
	}
	if got := models.ProjectCSP(&models.Project{Config: `{"csp":"x"}`}); got != "x" {
		t.Errorf("override: got %q, want x", got)
	}
}
