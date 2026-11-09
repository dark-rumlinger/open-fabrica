package public

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/server/middleware"
)

// TestAuthEndpointEnforcesConfiguredRateLimit proves the per-endpoint rate_limit
// set via manage_auth is actually enforced at request time (not merely
// advertised on the tool): with rate_limit=1 the first login attempt is
// processed (401 on bad creds) and a second within the same minute is rejected
// with 429 by the configured gate.
func TestAuthEndpointEnforcesConfiguredRateLimit(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE of_auth_endpoints (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			table_name TEXT, path TEXT UNIQUE,
			username_column TEXT DEFAULT 'username',
			password_column TEXT DEFAULT 'password',
			public_columns TEXT DEFAULT '[]',
			signup_columns TEXT DEFAULT '[]',
			jwt_expiry_hours INTEGER DEFAULT 24,
			default_role TEXT DEFAULT 'user',
			role_column TEXT DEFAULT 'role',
			rate_limit INTEGER DEFAULT 60,
			tenant_signup_mode TEXT DEFAULT 'none')`,
		`INSERT INTO of_auth_endpoints (table_name, path, username_column, password_column, public_columns, rate_limit)
			VALUES ('users', 'authrl', 'username', 'password', '[]', 1)`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, password TEXT, role TEXT DEFAULT '')`)

	h := &Handler{
		deps:        &Deps{},
		authLimiter: middleware.NewRateLimiter(5, 10),
		authLockout: middleware.NewAccountLockout(10, 30*time.Second, 15*time.Minute),
	}

	login := func() int {
		req := httptest.NewRequest("POST", "/api/authrl/login",
			strings.NewReader(`{"username":"nobody","password":"wrong"}`))
		rec := httptest.NewRecorder()
		if !h.handleAuthRequest(rec, req, 1, db, db, "authrl/login") {
			t.Fatalf("handleAuthRequest returned false; expected it to handle the auth route")
		}
		return rec.Code
	}

	// First attempt is within the configured budget (bad creds -> 401), and
	// consumes the single per-minute token.
	if code := login(); code == 429 {
		t.Fatalf("first login within budget returned 429; the configured limit should admit it")
	}

	// Second attempt within the same minute trips the configured rate_limit=1.
	if code := login(); code != 429 {
		t.Fatalf("second login = %d, want 429 (configured rate_limit=1 was not enforced)", code)
	}
}
