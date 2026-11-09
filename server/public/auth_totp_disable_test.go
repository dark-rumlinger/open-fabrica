package public

import (
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/security"
	"openfabrica/server/middleware"
)

// The second factor challenges an enrolled user only while TOTP is enabled on the
// auth endpoint. Once manage_2fa disable removes the endpoint row, the /totp routes
// answer 404 and login must stop asking for a code, otherwise enrolled users stay
// gated by a feature the owner turned off.
func TestLoginChallengesTOTPOnlyWhileEnabled(t *testing.T) {
	db := setupRowIDColumnDB(t,
		authEndpointsSchema,
		`INSERT INTO of_auth_endpoints (table_name, path, username_column, password_column, role_column, public_columns)
			VALUES ('members', 'acct', 'login_name', 'secret_hash', 'access_level', '["login_name"]')`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, login_name TEXT, secret_hash TEXT, access_level TEXT DEFAULT '')`,
		`CREATE TABLE of_totp_endpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT UNIQUE, auth_endpoint_path TEXT, issuer TEXT, enforced BOOLEAN DEFAULT 0, created_at TEXT DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE of_user_totp (user_id INTEGER PRIMARY KEY, auth_endpoint_path TEXT, secret_encrypted TEXT, verified BOOLEAN DEFAULT 0, verified_at TEXT)`)

	h := &Handler{
		deps:        &Deps{JWTManager: testAuthJWTManager()},
		authLimiter: middleware.NewRateLimiter(1000, 1000),
		authLockout: middleware.NewAccountLockout(100, time.Minute, time.Minute),
	}
	call := func(route, body string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/"+route, strings.NewReader(body))
		rec := httptest.NewRecorder()
		if !h.handleAuthRequest(rec, r, 1, db, db, route) {
			t.Fatalf("handleAuthRequest returned false for %q", route)
		}
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}

	creds := `{"login_name":"bob","password":"hunter2hunter2"}`
	if code, resp := call("acct/register", creds); code != http.StatusCreated {
		t.Fatalf("register: code=%d resp=%v", code, resp)
	}
	var userID int
	if err := db.QueryRow(`SELECT id FROM members WHERE login_name = 'bob'`).Scan(&userID); err != nil {
		t.Fatalf("read user id: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_user_totp (user_id, auth_endpoint_path, secret_encrypted, verified) VALUES (?, 'acct', 'sealed', 1)`, userID); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// No TOTP endpoint row yet: not enabled, so no challenge.
	if code, resp := call("acct/login", creds); code != http.StatusOK || resp["success"] != true {
		t.Fatalf("login with TOTP not enabled: code=%d resp=%v", code, resp)
	}

	if _, err := db.Exec(`INSERT INTO of_totp_endpoints (path, auth_endpoint_path, issuer) VALUES ('acct', 'acct', 'App')`); err != nil {
		t.Fatalf("enable totp: %v", err)
	}
	code, resp := call("acct/login", creds)
	if code != http.StatusOK || resp["needs_totp"] != true || resp["success"] == true {
		t.Fatalf("login with TOTP enabled must challenge an enrolled user: code=%d resp=%v", code, resp)
	}

	if _, err := db.Exec(`DELETE FROM of_totp_endpoints WHERE path = 'acct'`); err != nil {
		t.Fatalf("disable totp: %v", err)
	}
	if code, resp := call("acct/login", creds); code != http.StatusOK || resp["success"] != true {
		t.Fatalf("login after TOTP disabled must not challenge: code=%d resp=%v", code, resp)
	}
}

// With enforced=true an account that has not enrolled must not get a session from a
// correct password alone. Login hands out a single-purpose enrollment token, setup and
// verify accept it, and the first correct code is what signs the user in.
func TestEnforcedTOTPRequiresEnrollmentBeforeSession(t *testing.T) {
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	db := setupRowIDColumnDB(t,
		authEndpointsSchema,
		`INSERT INTO of_auth_endpoints (table_name, path, username_column, password_column, role_column, public_columns)
			VALUES ('members', 'acct', 'login_name', 'secret_hash', 'access_level', '["login_name"]')`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, login_name TEXT, secret_hash TEXT, access_level TEXT DEFAULT '')`,
		`CREATE TABLE of_totp_endpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT UNIQUE, auth_endpoint_path TEXT, issuer TEXT, enforced BOOLEAN DEFAULT 0, created_at TEXT DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE of_user_totp (user_id INTEGER PRIMARY KEY, auth_endpoint_path TEXT, secret_encrypted TEXT, verified BOOLEAN DEFAULT 0, verified_at TEXT)`,
		`CREATE TABLE of_magic_tokens (token TEXT PRIMARY KEY, auth_endpoint_path TEXT NOT NULL, email TEXT NOT NULL, user_id INTEGER, expires_at DATETIME NOT NULL, used BOOLEAN DEFAULT 0, purpose TEXT NOT NULL DEFAULT 'login', method TEXT NOT NULL DEFAULT '', created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`)

	h := &Handler{
		deps:              &Deps{JWTManager: testAuthJWTManager(), Encryptor: enc},
		authLimiter:       middleware.NewRateLimiter(1000, 1000),
		authLockout:       middleware.NewAccountLockout(100, time.Minute, time.Minute),
		totpVerifyLimiter: middleware.NewRateLimiter(1000, 1000),
	}
	call := func(method, route, body, bearer string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(method, "/api/"+route, strings.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		if !h.handleAuthRequest(rec, r, 1, db, db, route) {
			t.Fatalf("handleAuthRequest returned false for %q", route)
		}
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}

	creds := `{"login_name":"bob","password":"hunter2hunter2"}`
	if code, resp := call("POST", "acct/register", creds, ""); code != http.StatusCreated {
		t.Fatalf("register: code=%d resp=%v", code, resp)
	}

	// Enabled but NOT enforced: an unenrolled user still logs in with the password.
	if _, err := db.Exec(`INSERT INTO of_totp_endpoints (path, auth_endpoint_path, issuer, enforced) VALUES ('acct', 'acct', 'App', 0)`); err != nil {
		t.Fatal(err)
	}
	if code, resp := call("POST", "acct/login", creds, ""); code != http.StatusOK || resp["success"] != true {
		t.Fatalf("unenforced login of an unenrolled user: code=%d resp=%v", code, resp)
	}

	// Enforced: no session, an enrollment token instead.
	if _, err := db.Exec(`UPDATE of_totp_endpoints SET enforced = 1 WHERE path = 'acct'`); err != nil {
		t.Fatal(err)
	}
	code, resp := call("POST", "acct/login", creds, "")
	if code != http.StatusOK || resp["success"] == true || resp["needs_totp_setup"] != true {
		t.Fatalf("enforced login must ask for enrollment: code=%d resp=%v", code, resp)
	}
	if _, has := resp["token"]; has {
		t.Fatalf("enforced login of an unenrolled user issued a session token: %v", resp)
	}
	enroll, _ := resp["enroll_token"].(string)
	if enroll == "" {
		t.Fatalf("no enrollment token: %v", resp)
	}

	// The enrollment token is not a session: it must not open /me.
	if code, _ := call("GET", "acct/me", "", enroll); code != http.StatusUnauthorized {
		t.Errorf("enrollment token opened /me: code=%d", code)
	}

	code, resp = call("POST", "acct/totp/setup", "", enroll)
	if code != http.StatusOK || resp["success"] != true {
		t.Fatalf("setup with enrollment token: code=%d resp=%v", code, resp)
	}
	secret, _ := resp["secret"].(string)
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(raw) == 0 {
		t.Fatalf("setup returned no usable secret %q: %v", secret, err)
	}

	if code, _ := call("POST", "acct/totp/verify", `{"code":"000000"}`, enroll); code != http.StatusUnauthorized {
		t.Errorf("wrong code: code=%d, want 401", code)
	}
	good := security.TOTPCode(raw, time.Now().Unix()/30)
	code, resp = call("POST", "acct/totp/verify", `{"code":"`+good+`"}`, enroll)
	if code != http.StatusOK || resp["success"] != true {
		t.Fatalf("verify with the right code must sign in: code=%d resp=%v", code, resp)
	}
	token, _ := resp["token"].(string)
	if token == "" || resp["enrolled"] != true {
		t.Fatalf("verify did not return a session: %v", resp)
	}
	if code, _ := call("GET", "acct/me", "", token); code != http.StatusOK {
		t.Errorf("the session issued by enrollment does not open /me: code=%d", code)
	}

	// Single use.
	if code, _ := call("POST", "acct/totp/setup", "", enroll); code != http.StatusUnauthorized {
		t.Errorf("a used enrollment token was accepted again: code=%d", code)
	}

	// Enrolled now: the next login is the ordinary code challenge.
	code, resp = call("POST", "acct/login", creds, "")
	if code != http.StatusOK || resp["needs_totp"] != true {
		t.Fatalf("login after enrollment must challenge for a code: code=%d resp=%v", code, resp)
	}
}
