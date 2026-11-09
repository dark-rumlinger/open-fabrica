package security

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// okHandler records that it was reached and writes 200. Used as the "next"
// handler so tests can distinguish a middleware pass-through from a rejection.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthenticator_ValidBearerTokenPasses(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	token, err := jm.Generate(TokenRequest{UserID: 42, Username: "alice", Role: "user"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var reached bool
	h := jm.Authenticator(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !reached {
		t.Error("next handler was not reached for a valid token")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestAuthenticator_MissingTokenIs401(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	var reached bool
	h := jm.Authenticator(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if reached {
		t.Error("next handler should not be reached when the token is missing")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestAuthenticator_InvalidTokenIs401(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	var reached bool
	h := jm.Authenticator(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer this.is.not.a.valid.jwt")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if reached {
		t.Error("next handler should not be reached for an invalid token")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestAuthenticator_MalformedAuthHeaderIs401(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	var reached bool
	h := jm.Authenticator(okHandler(&reached))

	// No "Bearer " prefix -> invalid authorization format.
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Token abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if reached {
		t.Error("next handler should not be reached for a malformed header")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// The default Authenticator must NOT accept a ?token= query param: query-string
// tokens leak into logs/history/referrers, so only the SSE-scoped variant allows
// it.
func TestAuthenticator_TokenFromQueryParamIs401(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	token, err := jm.Generate(TokenRequest{ProjectID: 1, Username: "sse", Role: "user"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var reached bool
	h := jm.Authenticator(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/events?token="+token, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if reached {
		t.Error("default Authenticator must not accept a query-param token")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// AuthenticatorAllowingQueryToken is the SSE-scoped variant that DOES accept a
// valid ?token= query param.
func TestAuthenticatorAllowingQueryToken_QueryParamPasses(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	token, err := jm.Generate(TokenRequest{ProjectID: 1, Username: "sse", Role: "user"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var reached bool
	h := jm.AuthenticatorAllowingQueryToken(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/events/stream?token="+token, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !reached {
		t.Error("next handler was not reached for a valid query-param token on the SSE variant")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// The SSE variant still authenticates via the Authorization header and rejects a
// missing token, so it is a strict superset of Authenticator plus the query path.
func TestAuthenticatorAllowingQueryToken_MissingIs401(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	var reached bool
	h := jm.AuthenticatorAllowingQueryToken(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/events/stream", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if reached {
		t.Error("next handler should not be reached when no token is present")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestRequireRole_AllowsMatchingRole(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	token, err := jm.Generate(TokenRequest{UserID: 7, Username: "root", Role: "admin"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var reached bool
	// Authenticator populates claims in context; RequireRole then guards it.
	h := jm.Authenticator(RequireRole("admin")(okHandler(&reached)))

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if !reached {
		t.Error("next handler was not reached for a token with the required role")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

func TestRequireRole_DeniesMissingRole(t *testing.T) {
	jm := NewJWTManager("mw-secret", time.Hour)
	// Token carries role "user" but the guard requires "admin".
	token, err := jm.Generate(TokenRequest{UserID: 42, Username: "alice", Role: "user"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	var reached bool
	h := jm.Authenticator(RequireRole("admin")(okHandler(&reached)))

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if reached {
		t.Error("next handler should not be reached when the role is insufficient")
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func TestRequireRole_NoClaimsInContextIs403(t *testing.T) {
	// RequireRole used without Authenticator: no claims in context -> 403.
	var reached bool
	h := RequireRole("admin")(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if reached {
		t.Error("next handler should not be reached without claims")
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}
