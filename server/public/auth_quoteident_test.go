package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/security"
	"openfabrica/server/middleware"
)

// testAuthJWTManager builds a JWT manager with a fixed >=32-char secret for the
// auth handler tests.
func testAuthJWTManager() *security.JWTManager {
	return security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
}

// TestAuthReadPathsQuoteReservedWordColumns proves the auth read paths
// (login / me / refresh) wrap the configured table and column identifiers in
// security.QuoteIdent, so an auth table whose columns are SQLite RESERVED WORDS
// still authenticates. Without the quoting these become raw
// `SELECT id, check, group FROM members WHERE order = ?` - a SQL syntax error.
//
// The row is seeded directly and the handlers are invoked directly on purpose:
// both loadAuthEndpoint and the register body-column check reject reserved words
// up front, so this exercises the quoted SQL layer *beneath* those guards - the
// defense-in-depth this layer adds for any future path that reaches these queries with a
// less-validated config.
func TestAuthReadPathsQuoteReservedWordColumns(t *testing.T) {
	// Columns are SQLite reserved words, so the table itself must be created with
	// quoted identifiers.
	db := setupRowIDColumnDB(t,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, "order" TEXT, "check" TEXT, "group" TEXT DEFAULT 'user')`)

	const password = "pw-secret-123"
	hash, err := security.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO members ("order", "check", "group") VALUES (?, ?, ?)`, "alice", hash, "admin"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	h := &Handler{
		deps:        &Deps{JWTManager: testAuthJWTManager()},
		authLockout: middleware.NewAccountLockout(100, time.Minute, time.Minute),
	}
	ae := &authEndpointConfig{
		TableName:        "members",
		UsernameColumn:   "order",
		PasswordColumn:   "check",
		RoleColumn:       "group",
		DefaultRole:      "user",
		JWTExpiryHours:   24,
		TenantSignupMode: "none",
		PublicColumns:    []string{"order", "group"},
	}

	// login: SELECT id, "check", "group" FROM "members" WHERE "order" = ?
	loginRec := httptest.NewRecorder()
	loginReq := httptest.NewRequest("POST", "/api/acct/login",
		strings.NewReader(`{"order":"alice","password":"`+password+`"}`))
	h.authLogin(loginRec, loginReq, 1, db, db, "members", ae)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login with reserved-word columns: code=%d body=%s", loginRec.Code, loginRec.Body.String())
	}
	var loginResp map[string]any
	if err := json.Unmarshal(loginRec.Body.Bytes(), &loginResp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if loginResp["success"] != true {
		t.Fatalf("login success != true: %v", loginResp)
	}
	token, _ := loginResp["token"].(string)
	if token == "" {
		t.Fatalf("login returned no token: %v", loginResp)
	}

	// me: SELECT "order", "group" FROM "members" WHERE id = ?
	meRec := httptest.NewRecorder()
	meReq := httptest.NewRequest("GET", "/api/acct/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+token)
	h.authMe(meRec, meReq, 1, db, "members", ae)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me with reserved-word columns: code=%d body=%s", meRec.Code, meRec.Body.String())
	}
	var meResp map[string]any
	if err := json.Unmarshal(meRec.Body.Bytes(), &meResp); err != nil {
		t.Fatalf("decode me response: %v", err)
	}
	if meResp["order"] != "alice" {
		t.Errorf("me should surface the reserved-word username column, got %v", meResp)
	}
	// The platform helper gates on `role`; a custom role column is surfaced under it too.
	if meResp["role"] != "admin" || meResp["group"] != "admin" {
		t.Errorf("me should expose the custom role column as `role` as well, got %v", meResp)
	}

	// refresh: SELECT 1 / SELECT "group" FROM "members" WHERE id = ?
	refRec := httptest.NewRecorder()
	refReq := httptest.NewRequest("POST", "/api/acct/refresh", nil)
	refReq.Header.Set("Authorization", "Bearer "+token)
	h.authRefresh(refRec, refReq, 1, db, "members", ae)
	if refRec.Code != http.StatusOK {
		t.Fatalf("refresh with reserved-word columns: code=%d body=%s", refRec.Code, refRec.Body.String())
	}
}

// TestAuthCustomIdentifiersEndToEnd drives register -> login -> me through the
// real entry point (handleAuthRequest, including loadAuthEndpoint) with CUSTOM,
// non-default column and table names. It guards that wrapping the identifiers in
// QuoteIdent produces correct SQL on every path - the register INSERT and the
// login/me SELECTs - for a realistic config that does not use the default
// users(username,password,role) names.
func TestAuthCustomIdentifiersEndToEnd(t *testing.T) {
	db := setupRowIDColumnDB(t,
		authEndpointsSchema,
		`INSERT INTO of_auth_endpoints (table_name, path, username_column, password_column, role_column, public_columns)
			VALUES ('app_members', 'acct', 'login_name', 'secret_hash', 'access_level', '["login_name","access_level"]')`,
		`CREATE TABLE app_members (id INTEGER PRIMARY KEY AUTOINCREMENT, login_name TEXT, secret_hash TEXT, access_level TEXT DEFAULT '')`)

	h := &Handler{
		deps:        &Deps{JWTManager: testAuthJWTManager()},
		authLimiter: middleware.NewRateLimiter(1000, 1000),
		authLockout: middleware.NewAccountLockout(100, time.Minute, time.Minute),
	}

	authReq := func(method, route, body, token string) (int, map[string]any) {
		t.Helper()
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, "/api/"+route, nil)
		} else {
			r = httptest.NewRequest(method, "/api/"+route, strings.NewReader(body))
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		if !h.handleAuthRequest(rec, r, 1, db, db, route) {
			t.Fatalf("handleAuthRequest returned false for %q", route)
		}
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}

	// register: quoted INSERT INTO "app_members" (...) VALUES (...)
	if code, resp := authReq("POST", "acct/register", `{"login_name":"bob","password":"hunter2hunter2"}`, ""); code != http.StatusCreated || resp["success"] != true {
		t.Fatalf("register: code=%d resp=%v", code, resp)
	}

	// login: quoted SELECT finds the row, bcrypt check passes
	code, resp := authReq("POST", "acct/login", `{"login_name":"bob","password":"hunter2hunter2"}`, "")
	if code != http.StatusOK || resp["success"] != true {
		t.Fatalf("login: code=%d resp=%v", code, resp)
	}
	token, _ := resp["token"].(string)
	if token == "" {
		t.Fatalf("login returned no token: %v", resp)
	}

	// me: quoted SELECT of the custom public columns
	code, resp = authReq("GET", "acct/me", "", token)
	if code != http.StatusOK {
		t.Fatalf("me: code=%d resp=%v", code, resp)
	}
	if resp["login_name"] != "bob" {
		t.Errorf("me should return login_name=bob via the custom username column, got %v", resp)
	}
}
