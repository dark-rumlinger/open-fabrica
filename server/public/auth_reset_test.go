package public

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/security"
	"openfabrica/server/middleware"
)

func resetFixture(t *testing.T) (*Handler, *sql.DB, *authEndpointConfig) {
	t.Helper()
	db := setupRowIDColumnDB(t,
		`CREATE TABLE of_magic_tokens (token TEXT PRIMARY KEY, auth_endpoint_path TEXT NOT NULL, email TEXT NOT NULL, user_id INTEGER, expires_at DATETIME NOT NULL, used BOOLEAN DEFAULT 0, purpose TEXT NOT NULL DEFAULT 'login', method TEXT NOT NULL DEFAULT '', created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE of_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, type TEXT, payload TEXT, status TEXT, max_attempts INTEGER)`,
		`CREATE TABLE of_email_config (id INTEGER PRIMARY KEY, provider_name TEXT)`,
		`CREATE TABLE of_email_templates (id INTEGER PRIMARY KEY, name TEXT, subject TEXT, body_html TEXT)`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, password TEXT, role TEXT)`,
		`INSERT INTO of_email_config (id, provider_name) VALUES (1, 'smtp')`,
	)
	db.SetMaxOpenConns(1)
	oldHash, err := security.HashPassword("old-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO members (email, password, role) VALUES ('ada@x.io', ?, 'user')`, oldHash); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		deps:               &Deps{JWTManager: security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)},
		authLimiter:        middleware.NewRateLimiter(100, 100),
		magicEmailLimiter:  middleware.NewRateLimiter(100, 100),
		magicVerifyLimiter: middleware.NewRateLimiter(100, 100),
		authLockout:        middleware.NewAccountLockout(5, time.Minute, time.Hour),
	}
	ae := &authEndpointConfig{TableName: "members", Path: "auth", UsernameColumn: "email", PasswordColumn: "password", RoleColumn: "role", DefaultRole: "user"}
	return h, db, ae
}

func forgot(t *testing.T, h *Handler, db *sql.DB, ae *authEndpointConfig, email, host string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/forgot", strings.NewReader(`{"email":"`+email+`"}`))
	req.Host = host
	rec := httptest.NewRecorder()
	h.handlePassword(rec, req, 1, db, db, "members", ae, "forgot")
	return rec.Code
}

func queuedLink(t *testing.T, db *sql.DB) string {
	t.Helper()
	var payload string
	if err := db.QueryRow(`SELECT payload FROM of_jobs ORDER BY id DESC LIMIT 1`).Scan(&payload); err != nil {
		return ""
	}
	i := strings.Index(payload, "token=")
	if i < 0 {
		return ""
	}
	return payload[i+len("token=") : i+len("token=")+64]
}

func TestPasswordResetFlow(t *testing.T) {
	h, db, ae := resetFixture(t)

	if code := forgot(t, h, db, ae, "ada@x.io", "localhost:5733"); code != http.StatusOK {
		t.Fatalf("forgot: %d", code)
	}
	token := queuedLink(t, db)
	if len(token) != 64 {
		t.Fatalf("no reset link queued: %q", token)
	}
	var stored string
	db.QueryRow(`SELECT token FROM of_magic_tokens WHERE purpose = 'reset'`).Scan(&stored)
	if stored == token || stored != magicTokenHash(token) {
		t.Error("the reset token must be stored hashed")
	}

	reset := func(tok, pw string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/password/reset", strings.NewReader(`{"token":"`+tok+`","password":"`+pw+`"}`))
		rec := httptest.NewRecorder()
		h.handlePassword(rec, req, 1, db, db, "members", ae, "reset")
		return rec.Code
	}
	if code := reset("0000", "new-password"); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d, want 401", code)
	}
	if code := reset(token, strings.Repeat("x", 80)); code != http.StatusBadRequest {
		t.Errorf("over-long password: %d, want 400", code)
	}
	if code := reset(token, "new-password"); code != http.StatusOK {
		t.Fatalf("reset: %d", code)
	}
	var hash string
	db.QueryRow(`SELECT password FROM members WHERE email = 'ada@x.io'`).Scan(&hash)
	if !security.CheckPassword("new-password", hash) || security.CheckPassword("old-password", hash) {
		t.Error("the password was not replaced")
	}
	if code := reset(token, "another-password"); code != http.StatusUnauthorized {
		t.Errorf("reusing the link: %d, want 401", code)
	}
}

// A magic-link login token must not double as a reset token, nor the reverse.
func TestResetAndLoginTokensAreNotInterchangeable(t *testing.T) {
	h, db, ae := resetFixture(t)
	forgot(t, h, db, ae, "ada@x.io", "localhost:5733")
	token := queuedLink(t, db)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/magic/verify?token="+token, nil)
	rec := httptest.NewRecorder()
	h.magicVerify(rec, req, 1, db, db, "members", ae, "")
	if rec.Code == http.StatusOK {
		t.Fatal("a reset token signed someone in as a magic link")
	}
}

func TestForgotNeverRevealsWhoHasAnAccountOrMailsAForgedHost(t *testing.T) {
	h, db, ae := resetFixture(t)

	if code := forgot(t, h, db, ae, "nobody@x.io", "localhost:5733"); code != http.StatusOK {
		t.Errorf("unknown address: %d, want the same 200", code)
	}
	var jobs int
	db.QueryRow(`SELECT COUNT(*) FROM of_jobs`).Scan(&jobs)
	if jobs != 0 {
		t.Error("an email was queued for an address with no account")
	}

	// A forged Host header must not become the origin in the victim's link.
	if code := forgot(t, h, db, ae, "ada@x.io", "evil.example"); code != http.StatusOK {
		t.Errorf("forged host: %d", code)
	}
	db.QueryRow(`SELECT COUNT(*) FROM of_jobs`).Scan(&jobs)
	if jobs != 0 {
		t.Error("a reset email was queued for a request carrying a forged Host header")
	}
}
