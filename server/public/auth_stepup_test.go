package public

import (
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"openfabrica/events"
	"openfabrica/security"
	"openfabrica/server/middleware"
)

// stepUpProject keeps these tests' TOTP replay state (keyed by project and user) apart
// from other tests that verify a code for user 1 in the same 30s window.
const stepUpProject = 4242

// stepUpFixture is a project with an auth endpoint "acct" on members(email), magic
// links and TOTP enabled, and one ENROLLED user (eve@x.io, id 1) whose TOTP secret is
// returned so the test can compute valid codes.
func stepUpFixture(t *testing.T) (*Handler, *sql.DB, *authEndpointConfig, []byte, *[]events.Event) {
	t.Helper()
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	db := setupRowIDColumnDB(t,
		authEndpointsSchema,
		`INSERT INTO of_auth_endpoints (table_name, path, username_column, password_column, role_column, public_columns)
			VALUES ('members', 'acct', 'email', 'password', 'role', '["email"]')`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, password TEXT, role TEXT)`,
		`CREATE TABLE of_magic_tokens (token TEXT PRIMARY KEY, auth_endpoint_path TEXT NOT NULL, email TEXT NOT NULL, user_id INTEGER, expires_at DATETIME NOT NULL, used BOOLEAN DEFAULT 0, purpose TEXT NOT NULL DEFAULT 'login', method TEXT NOT NULL DEFAULT '', created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE of_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, type TEXT, payload TEXT, status TEXT, max_attempts INTEGER)`,
		totpEndpointsDDL, userTOTPDDL,
		`INSERT INTO of_totp_endpoints (path, auth_endpoint_path, issuer, enforced) VALUES ('acct', 'acct', 'App', 0)`,
		`INSERT INTO members (id, email, password, role) VALUES (1, 'eve@x.io', 'x', 'user')`,
	)
	db.SetMaxOpenConns(1)

	secret := security.NewTOTPSecret()
	sealed, err := enc.Encrypt(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_user_totp (user_id, auth_endpoint_path, secret_encrypted, verified) VALUES (1, 'acct', ?, 1)`, sealed); err != nil {
		t.Fatal(err)
	}
	raw, err := totpEncodingForTest(secret)
	if err != nil {
		t.Fatal(err)
	}

	bus := events.NewBus()
	var got []events.Event
	bus.SubscribeAll(func(e events.Event) { got = append(got, e) })
	h := &Handler{
		deps:              &Deps{JWTManager: testAuthJWTManager(), Encryptor: enc, Bus: bus},
		authLimiter:       middleware.NewRateLimiter(1000, 1000),
		authLockout:       middleware.NewAccountLockout(100, time.Minute, time.Minute),
		totpVerifyLimiter: middleware.NewRateLimiter(1000, 1000),
	}
	ae := &authEndpointConfig{TableName: "members", Path: "acct", UsernameColumn: "email", PasswordColumn: "password", RoleColumn: "role", DefaultRole: "user"}
	return h, db, ae, raw, &got
}

func totpEncodingForTest(secret string) ([]byte, error) {
	return totpEncodingNoPad.DecodeString(secret)
}

// magicLinkToken requests a magic link for email and returns the raw token from the
// queued email.
func magicLinkToken(t *testing.T, h *Handler, db *sql.DB, ae *authEndpointConfig, email string) string {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM of_jobs`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/acct/magic/request", strings.NewReader(`{"email":"`+email+`"}`))
	req.Host = "localhost:5733"
	h.magicRequest(httptest.NewRecorder(), req, stepUpProject, db, db, "members", ae, 15, "")
	var payload string
	if err := db.QueryRow(`SELECT payload FROM of_jobs`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	i := strings.Index(payload, "token=")
	if i < 0 {
		t.Fatalf("no token in queued email: %s", payload)
	}
	return payload[i+len("token=") : i+len("token=")+64]
}

// TestMagicLinkOfAnEnrolledUserRequiresTheirCode pins the second factor on a magic-link
// sign-in: no session until the enrolled user's TOTP code is verified, and the verify
// call is what signs them in (auth.login carries method magic_link).
func TestMagicLinkOfAnEnrolledUserRequiresTheirCode(t *testing.T) {
	h, db, ae, raw, got := stepUpFixture(t)

	// Browser flow: redirected to /login with a step-up token, never an auth token.
	r := httptest.NewRequest(http.MethodGet, "/api/acct/magic/verify?token="+magicLinkToken(t, h, db, ae, "eve@x.io"), nil)
	r.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.magicVerify(rec, r, stepUpProject, db, db, "members", ae, "")
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.HasPrefix(loc, "/login#totp_verify=") || strings.Contains(loc, "auth_token") {
		t.Fatalf("magic link of an enrolled user: status %d, Location %q; want a redirect to /login#totp_verify=", rec.Code, loc)
	}
	stepToken := strings.TrimPrefix(loc, "/login#totp_verify=")
	for _, e := range *got {
		if e.Type == events.EventAuthLogin {
			t.Fatal("auth.login fired before the second factor was verified")
		}
	}

	verify := func(code, bearer string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/acct/totp/verify", strings.NewReader(`{"code":"`+code+`"}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		if !h.handleAuthRequest(w, req, stepUpProject, db, db, "acct/totp/verify") {
			t.Fatal("handleAuthRequest did not route totp/verify")
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		return w.Code, resp
	}
	// The step-up token is not a session.
	meReq := httptest.NewRequest(http.MethodGet, "/api/acct/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+stepToken)
	meRec := httptest.NewRecorder()
	h.handleAuthRequest(meRec, meReq, stepUpProject, db, db, "acct/me")
	if meRec.Code != http.StatusUnauthorized {
		t.Fatalf("the step-up token opened /me: %d", meRec.Code)
	}
	if code, _ := verify("000000", stepToken); code != http.StatusUnauthorized {
		t.Fatalf("a wrong code: status %d, want 401", code)
	}
	code, resp := verify(security.TOTPCode(raw, time.Now().Unix()/30), stepToken)
	if code != http.StatusOK || resp["success"] != true || resp["token"] == nil {
		t.Fatalf("the right code must sign in: status %d resp %v", code, resp)
	}
	if _, has := resp["enrolled"]; has {
		t.Errorf("a login step-up reported enrollment: %v", resp)
	}
	var login *events.Event
	for i := range *got {
		if (*got)[i].Type == events.EventAuthLogin {
			login = &(*got)[i]
		}
	}
	if login == nil || login.Payload["method"] != authMethodMagicLink {
		t.Fatalf("auth.login after the step-up: %v, want method magic_link", login)
	}
	// Single use.
	if code, _ := verify(security.TOTPCode(raw, time.Now().Unix()/30), stepToken); code != http.StatusUnauthorized {
		t.Errorf("a used step-up token was accepted again: %d", code)
	}

	// JSON flow: the step-up shape, no token.
	rec = httptest.NewRecorder()
	h.magicVerify(rec, httptest.NewRequest(http.MethodGet, "/api/acct/magic/verify?token="+magicLinkToken(t, h, db, ae, "eve@x.io"), nil), stepUpProject, db, db, "members", ae, "")
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["needs_totp_verify"] != true || body["totp_token"] == nil || body["token"] != nil {
		t.Fatalf("JSON magic verify of an enrolled user: %v", body)
	}
}

// TestMagicLinkOnAnEnforcedEndpointRequiresEnrollment pins the enforced case: a user
// who has not enrolled gets the enrollment step, not a session.
func TestMagicLinkOnAnEnforcedEndpointRequiresEnrollment(t *testing.T) {
	h, db, ae, _, _ := stepUpFixture(t)
	if _, err := db.Exec(`UPDATE of_totp_endpoints SET enforced = 1`); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/acct/magic/verify?token="+magicLinkToken(t, h, db, ae, "new@x.io"), nil)
	r.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.magicVerify(rec, r, stepUpProject, db, db, "members", ae, "")
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.HasPrefix(loc, "/login#totp_setup=") {
		t.Fatalf("enforced endpoint, unenrolled user: status %d, Location %q; want /login#totp_setup=", rec.Code, loc)
	}
	if u, err := url.Parse(loc); err != nil || strings.Contains(u.Fragment, "auth_token") {
		t.Fatalf("a session token leaked into %q", loc)
	}
}

// TestMagicLinkWithoutTOTPSignsInDirectly keeps the ordinary path: an endpoint without
// 2FA, or an unenrolled user on a non-enforced one, signs in straight away.
func TestMagicLinkWithoutTOTPSignsInDirectly(t *testing.T) {
	h, db, ae, _, _ := stepUpFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/acct/magic/verify?token="+magicLinkToken(t, h, db, ae, "new@x.io"), nil)
	r.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.magicVerify(rec, r, stepUpProject, db, db, "members", ae, "")
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || !strings.Contains(loc, "#auth_token=") {
		t.Fatalf("unenrolled user on a non-enforced endpoint: status %d, Location %q; want a session", rec.Code, loc)
	}
}

// totpEncodingNoPad is the unpadded base32 authenticator secrets use.
var totpEncodingNoPad = base32.StdEncoding.WithPadding(base32.NoPadding)
