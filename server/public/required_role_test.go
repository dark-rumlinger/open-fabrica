package public

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/db/models"
	"openfabrica/security"
)

// required_role is matched by name, except that admin satisfies every gate (as it
// already bypasses owner scoping), so an admin is not locked out of an editors-only
// endpoint.
func TestRequiredRoleGate(t *testing.T) {
	jwt := security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
	h := &Handler{deps: &Deps{JWTManager: jwt}}
	ep := &apiEndpoint{RequiresAuth: true, RequiredRole: "editor"}

	status := func(role string) int {
		token, err := jwt.Generate(security.TokenRequest{ProjectID: 1, UserID: 3, Username: "u", Role: role})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		if _, _, ok := h.buildAPIRequestContext(rec, req, &models.Project{ID: 1}, ep); ok {
			return http.StatusOK
		}
		return rec.Code
	}
	if got := status("editor"); got != http.StatusOK {
		t.Errorf("editor: %d, want 200", got)
	}
	if got := status("admin"); got != http.StatusOK {
		t.Errorf("admin: %d, want 200", got)
	}
	if got := status("viewer"); got != http.StatusForbidden {
		t.Errorf("viewer: %d, want 403", got)
	}
}
