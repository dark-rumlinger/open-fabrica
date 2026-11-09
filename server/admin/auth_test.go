package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
	"openfabrica/server/middleware"
)

// usersTestSchema mirrors the users table from db/migrations/001_initial.sql -
// just enough for the login and admin-guard paths (GetUserByUsername /
// GetUserTokenVersion).
const usersTestSchema = `
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT 'admin',
    token_version INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);`

const testAdminPassword = "correct-horse-battery-staple"

// newAuthTestHandler builds an AuthHandler backed by an in-memory DB seeded with
// one admin user ("admin" / testAdminPassword). Returns the handler, the raw DB
// (to tweak token_version), and the seeded user.
func newAuthTestHandler(t *testing.T) (*AuthHandler, *sql.DB, *models.User) {
	t.Helper()
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	if _, err := mem.Exec(usersTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	hash, err := security.HashPassword(testAdminPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := models.CreateUser(mem, "admin", hash, "admin", "Admin"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	user, err := models.GetUserByUsername(mem, "admin")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	deps := &Deps{Core: sdeps.Core{
		DB:         &db.DB{DB: mem},
		JWTManager: security.NewJWTManager("test-jwt-secret-at-least-32-bytes-long", time.Hour),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	h := &AuthHandler{
		deps:    deps,
		lockout: middleware.NewAccountLockout(middleware.AdminLockoutAttempts, time.Minute, time.Minute),
	}
	return h, mem, user
}

func postLogin(t *testing.T, h *AuthHandler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(loginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleLogin(rec, req)
	return rec
}

func TestAdminLogin_Success(t *testing.T) {
	h, _, _ := newAuthTestHandler(t)

	rec := postLogin(t, h, "admin", testAdminPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Error("expected a non-empty token")
	}
	if resp.User.Username != "admin" || resp.User.Role != "admin" {
		t.Errorf("user view = %+v, want username=admin role=admin", resp.User)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Error("expected an auth cookie to be set")
	}
}

// TestAdminLogin_WrongPasswordAndEnumerationParity confirms both a wrong password
// and an unknown username return an identical 401 response, so an attacker can't
// tell which usernames exist (the DummyPasswordCompare timing mitigation has a
// visible-behavior counterpart: the response itself must not differ).
func TestAdminLogin_WrongPasswordAndEnumerationParity(t *testing.T) {
	h, _, _ := newAuthTestHandler(t)

	wrong := postLogin(t, h, "admin", "not-the-password")
	if wrong.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: status = %d, want 401", wrong.Code)
	}
	ghost := postLogin(t, h, "does-not-exist", "whatever")
	if ghost.Code != http.StatusUnauthorized {
		t.Errorf("unknown user: status = %d, want 401", ghost.Code)
	}
	if strings.TrimSpace(wrong.Body.String()) != strings.TrimSpace(ghost.Body.String()) {
		t.Errorf("response differs between wrong-password and unknown-user (enumeration oracle):\n wrong: %s\n ghost: %s",
			wrong.Body.String(), ghost.Body.String())
	}
}

func TestAdminLogin_EmptyCredentials(t *testing.T) {
	h, _, _ := newAuthTestHandler(t)

	rec := postLogin(t, h, "", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestAdminLogin_LockoutAfterThreshold confirms the per-username lockout kicks in
// after AdminLockoutAttempts failures and, crucially, is checked BEFORE the
// password - so once locked, even the correct password is refused with 429.
func TestAdminLogin_LockoutAfterThreshold(t *testing.T) {
	h, _, _ := newAuthTestHandler(t)

	for i := 0; i < middleware.AdminLockoutAttempts; i++ {
		rec := postLogin(t, h, "admin", "wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}

	locked := postLogin(t, h, "admin", "wrong")
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: status = %d, want 429", middleware.AdminLockoutAttempts, locked.Code)
	}
	if locked.Header().Get("Retry-After") == "" {
		t.Error("expected a Retry-After header on lockout")
	}

	// The correct password is still refused while the lock window is open.
	correct := postLogin(t, h, "admin", testAdminPassword)
	if correct.Code != http.StatusTooManyRequests {
		t.Errorf("correct password while locked: status = %d, want 429", correct.Code)
	}
}

// TestAdminAuthGuard exercises the middleware that gates every protected admin
// route: it must accept only an unscoped (ProjectID==0) token whose version still
// matches the DB, and reject everything else.
func TestAdminAuthGuard(t *testing.T) {
	h, mem, user := newAuthTestHandler(t)
	guard := adminAuthGuard(h.deps)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	call := func(claims *security.Claims) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/system", nil)
		if claims != nil {
			req = req.WithContext(context.WithValue(req.Context(), security.ClaimsKey, claims))
		}
		rec := httptest.NewRecorder()
		guard(next).ServeHTTP(rec, req)
		return rec
	}

	// Valid unscoped admin token whose version matches the DB -> passes through.
	if rec := call(&security.Claims{ProjectID: 0, UserID: user.ID, Username: "admin", Role: "admin", TokenVersion: user.TokenVersion}); rec.Code != http.StatusOK {
		t.Errorf("valid admin token: status = %d, want 200", rec.Code)
	}
	// No claims in context -> 401.
	if rec := call(nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing claims: status = %d, want 401", rec.Code)
	}
	// Project-scoped token (ProjectID != 0) -> 403, even carrying role "admin".
	if rec := call(&security.Claims{ProjectID: 7, UserID: user.ID, Role: "admin"}); rec.Code != http.StatusForbidden {
		t.Errorf("project-scoped token: status = %d, want 403", rec.Code)
	}
	// Unknown user id -> 401 (token_version lookup fails).
	if rec := call(&security.Claims{ProjectID: 0, UserID: 999999, TokenVersion: 0}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown user: status = %d, want 401", rec.Code)
	}
	// Bump the stored token_version: a token minted at the old version is stale.
	if _, err := mem.Exec("UPDATE users SET token_version = token_version + 1 WHERE id = ?", user.ID); err != nil {
		t.Fatalf("bump token_version: %v", err)
	}
	if rec := call(&security.Claims{ProjectID: 0, UserID: user.ID, TokenVersion: user.TokenVersion}); rec.Code != http.StatusUnauthorized {
		t.Errorf("stale token_version: status = %d, want 401", rec.Code)
	}
}

// putAuth sends a request to an authenticated auth handler as the given user.
func putAuth(t *testing.T, handler http.HandlerFunc, method, body string, user *models.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/admin/api/auth/x", strings.NewReader(body))
	claims := &security.Claims{UserID: user.ID, Username: user.Username, Role: user.Role, TokenVersion: user.TokenVersion}
	req = req.WithContext(context.WithValue(req.Context(), security.ClaimsKey, claims))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestChangePassword(t *testing.T) {
	h, mem, user := newAuthTestHandler(t)
	const newPassword = "N3w-passphrase!"
	change := func(cur, next string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(changePasswordRequest{CurrentPassword: cur, NewPassword: next})
		return putAuth(t, h.HandleChangePassword, http.MethodPut, string(b), user)
	}

	if rec := change("wrong-password", newPassword); rec.Code != http.StatusBadRequest {
		t.Errorf("wrong current password: status = %d, want 400", rec.Code)
	}
	if rec := change(testAdminPassword, "weak"); rec.Code != http.StatusBadRequest {
		t.Errorf("weak new password: status = %d, want 400", rec.Code)
	}
	if rec := putAuth(t, h.HandleChangePassword, http.MethodPut, `{}`, user); rec.Code != http.StatusBadRequest {
		t.Errorf("missing fields: status = %d, want 400", rec.Code)
	}
	if rec := change(testAdminPassword, newPassword); rec.Code != http.StatusOK {
		t.Fatalf("valid change: status = %d, body %s", rec.Code, rec.Body.String())
	} else {
		var resp loginResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" {
			t.Fatalf("response lacks a fresh token: %v %s", err, rec.Body.String())
		}
		claims, err := h.deps.JWTManager.Validate(resp.Token)
		if err != nil {
			t.Fatalf("fresh token invalid: %v", err)
		}
		var current int
		_ = mem.QueryRow("SELECT token_version FROM users WHERE id = ?", user.ID).Scan(&current)
		if claims.TokenVersion != current || current != user.TokenVersion+1 {
			t.Errorf("fresh token version = %d, db = %d, want %d", claims.TokenVersion, current, user.TokenVersion+1)
		}
	}

	if rec := postLogin(t, h, "admin", testAdminPassword); rec.Code != http.StatusUnauthorized {
		t.Errorf("old password still works: status = %d", rec.Code)
	}
	if rec := postLogin(t, h, "admin", newPassword); rec.Code != http.StatusOK {
		t.Errorf("new password rejected: status = %d", rec.Code)
	}
}

func TestChangePasswordCountsTowardLockout(t *testing.T) {
	h, _, user := newAuthTestHandler(t)
	b, _ := json.Marshal(changePasswordRequest{CurrentPassword: "wrong-password", NewPassword: "N3w-passphrase!"})
	var last int
	for i := 0; i < middleware.AdminLockoutAttempts+1; i++ {
		last = putAuth(t, h.HandleChangePassword, http.MethodPut, string(b), user).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("status after repeated wrong passwords = %d, want 429", last)
	}
}

func TestRevokeSessionsInvalidatesOldTokensOnly(t *testing.T) {
	h, _, user := newAuthTestHandler(t)
	guard := adminAuthGuard(h.deps)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	call := func(token string) int {
		claims, err := h.deps.JWTManager.Validate(token)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/admin/api/x", nil)
		req = req.WithContext(context.WithValue(req.Context(), security.ClaimsKey, claims))
		rec := httptest.NewRecorder()
		guard(next).ServeHTTP(rec, req)
		return rec.Code
	}

	old, err := h.deps.JWTManager.Generate(security.TokenRequest{UserID: user.ID, Username: user.Username, Role: user.Role, TokenVersion: user.TokenVersion})
	if err != nil {
		t.Fatal(err)
	}
	if got := call(old); got != http.StatusOK {
		t.Fatalf("token before revoke: status = %d, want 200", got)
	}

	rec := putAuth(t, h.HandleRevokeSessions, http.MethodPost, ``, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		t.Fatalf("revoke response lacks a token: %v", err)
	}
	if got := call(old); got != http.StatusUnauthorized {
		t.Errorf("old token after revoke: status = %d, want 401", got)
	}
	if got := call(resp.Token); got != http.StatusOK {
		t.Errorf("fresh token after revoke: status = %d, want 200", got)
	}
}

// newFirstRunHandler builds an AuthHandler over an empty users table whose admin
// panel is bound to the given address.
func newFirstRunHandler(t *testing.T, bindAddr string) *AuthHandler {
	t.Helper()
	mem, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	if _, err := mem.Exec(usersTestSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	deps := &Deps{Core: sdeps.Core{
		Config:     &config.Config{AdminBindAddr: bindAddr},
		DB:         &db.DB{DB: mem},
		JWTManager: security.NewJWTManager("test-jwt-secret-at-least-32-bytes-long", time.Hour),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	h := &AuthHandler{deps: deps, lockout: middleware.NewAccountLockout(middleware.AdminLockoutAttempts, time.Minute, time.Minute)}
	h.armSetupToken()
	return h
}

func setupCheck(t *testing.T, h *AuthHandler) map[string]bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.HandleSetupCheck(rec, httptest.NewRequest(http.MethodGet, "/admin/api/setup/check", nil))
	var got map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// A loopback-bound instance needs no setup token: only a local user can reach it.
func TestSetupNeedsNoTokenOnLoopback(t *testing.T) {
	h := newFirstRunHandler(t, "127.0.0.1")
	if got := setupCheck(t, h); !got["needs_setup"] || got["needs_setup_token"] {
		t.Errorf("setup/check = %v, want needs_setup without a token", got)
	}
	if !h.requireSetupToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/api/setup", nil)) {
		t.Error("loopback setup was refused")
	}
}

// An exposed, unclaimed instance refuses every setup call without the console
// token, accepts it with the token, and stops asking once the box is claimed.
func TestSetupTokenGuardsExposedInstance(t *testing.T) {
	h := newFirstRunHandler(t, "0.0.0.0")
	if got := setupCheck(t, h); !got["needs_setup"] || !got["needs_setup_token"] {
		t.Fatalf("setup/check = %v, want needs_setup_token", got)
	}
	tok := *h.setupToken.Load()

	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"setup":     h.HandleSetup,
		"providers": h.HandleProviderCatalog,
	} {
		rec := httptest.NewRecorder()
		call(rec, httptest.NewRequest(http.MethodPost, "/admin/api/x", strings.NewReader(`{}`)))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without a token: status = %d, want 403", name, rec.Code)
		}
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/x", strings.NewReader(`{}`))
		req.Header.Set(setupTokenHeader, "wrong-"+tok)
		call(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with a wrong token: status = %d, want 403", name, rec.Code)
		}
	}

	// With the right token the guard lets setup proceed to its own validation.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/setup", strings.NewReader(`{"username":"","password":""}`))
	req.Header.Set(setupTokenHeader, tok)
	h.HandleSetup(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("setup with the token: status = %d, want 400 from credential validation", rec.Code)
	}

	// A completed setup consumes the token.
	h.setupToken.Store(nil)
	if !h.requireSetupToken(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/admin/api/setup", nil)) {
		t.Error("a consumed token still blocks")
	}
}

// The wizard checks the token as soon as it is typed: wrong is 403, right is 200,
// and an instance that already has an admin answers 410.
func TestSetupTokenCheckEndpoint(t *testing.T) {
	h := newFirstRunHandler(t, "0.0.0.0")
	tok := *h.setupToken.Load()
	call := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/setup/token", nil)
		if token != "" {
			req.Header.Set(setupTokenHeader, token)
		}
		rec := httptest.NewRecorder()
		h.HandleSetupTokenCheck(rec, req)
		return rec.Code
	}
	if got := call(""); got != http.StatusForbidden {
		t.Errorf("no token: status = %d, want 403", got)
	}
	if got := call("not-" + tok); got != http.StatusForbidden {
		t.Errorf("wrong token: status = %d, want 403", got)
	}
	if got := call(tok); got != http.StatusOK {
		t.Errorf("right token: status = %d, want 200", got)
	}

	claimed, _, _ := newAuthTestHandler(t)
	rec := httptest.NewRecorder()
	claimed.HandleSetupTokenCheck(rec, httptest.NewRequest(http.MethodPost, "/admin/api/setup/token", nil))
	if rec.Code != http.StatusGone {
		t.Errorf("claimed instance: status = %d, want 410", rec.Code)
	}
}

// An instance that already has an admin never arms a token.
func TestSetupTokenNotArmedOnceClaimed(t *testing.T) {
	h, _, _ := newAuthTestHandler(t)
	h.deps.Config = &config.Config{AdminBindAddr: "0.0.0.0"}
	h.armSetupToken()
	if h.setupToken.Load() != nil {
		t.Error("a token was armed on an instance that already has an admin")
	}
}
