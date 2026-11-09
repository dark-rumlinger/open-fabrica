package public

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/events"
	"openfabrica/security"
	"openfabrica/server/middleware"
)

// The TOTP tables every project database has; a sign-in reads them for its second factor.
const (
	totpEndpointsDDL = `CREATE TABLE of_totp_endpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT UNIQUE, auth_endpoint_path TEXT, issuer TEXT, enforced BOOLEAN DEFAULT 0, created_at TEXT DEFAULT CURRENT_TIMESTAMP)`
	userTOTPDDL      = `CREATE TABLE of_user_totp (user_id INTEGER PRIMARY KEY, auth_endpoint_path TEXT, secret_encrypted TEXT, verified BOOLEAN DEFAULT 0, verified_at TEXT)`
)

func magicTestDB(t *testing.T) *Handler {
	t.Helper()
	return &Handler{deps: &Deps{JWTManager: security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)}}
}

func TestMagicVerifyIsSingleUseAndStoresOnlyTheHash(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE of_magic_tokens (token TEXT PRIMARY KEY, auth_endpoint_path TEXT NOT NULL, email TEXT NOT NULL, user_id INTEGER, expires_at DATETIME NOT NULL, used BOOLEAN DEFAULT 0, purpose TEXT NOT NULL DEFAULT 'login', method TEXT NOT NULL DEFAULT '', created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE of_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, type TEXT, payload TEXT, status TEXT, max_attempts INTEGER)`,
		`CREATE TABLE of_magic_link_endpoints (path TEXT, email_template TEXT, redirect_url TEXT, ttl_minutes INTEGER)`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, password TEXT, role TEXT)`,
		totpEndpointsDDL, userTOTPDDL,
	)
	db.SetMaxOpenConns(1)
	h := magicTestDB(t)
	ae := &authEndpointConfig{TableName: "members", Path: "auth", UsernameColumn: "email", PasswordColumn: "password", RoleColumn: "role", DefaultRole: "user"}

	// Request: capture the raw token from the queued email.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic/request", strings.NewReader(`{"email":"new@x.io"}`))
	req.Host = "localhost:5733"
	rec := httptest.NewRecorder()
	h.magicRequest(rec, req, 1, db, db, "members", ae, 15, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("request status %d: %s", rec.Code, rec.Body.String())
	}
	var payload string
	if err := db.QueryRow(`SELECT payload FROM of_jobs`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	i := strings.Index(payload, "token=")
	if i < 0 {
		t.Fatalf("no token in queued email: %s", payload)
	}
	raw := payload[i+len("token="):]
	raw = raw[:64]

	var stored string
	if err := db.QueryRow(`SELECT token FROM of_magic_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == raw || stored != magicTokenHash(raw) {
		t.Fatalf("table must hold the hash, not the raw token (stored %q)", stored)
	}

	redeem := func() int {
		r := httptest.NewRequest(http.MethodGet, "/api/auth/magic/verify?token="+raw, nil)
		w := httptest.NewRecorder()
		h.magicVerify(w, r, 1, db, db, "members", ae, "")
		return w.Code
	}
	if code := redeem(); code != http.StatusOK {
		t.Fatalf("first redemption: status %d", code)
	}
	if code := redeem(); code != http.StatusUnauthorized {
		t.Fatalf("second redemption: status %d, want 401", code)
	}
	var users int
	db.QueryRow(`SELECT COUNT(*) FROM members`).Scan(&users)
	if users != 1 {
		t.Errorf("%d accounts after two redemptions, want 1", users)
	}
}

func TestMagicRequestThrottlesPerAddress(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE of_magic_tokens (token TEXT PRIMARY KEY, auth_endpoint_path TEXT NOT NULL, email TEXT NOT NULL, user_id INTEGER, expires_at DATETIME NOT NULL, used BOOLEAN DEFAULT 0, purpose TEXT NOT NULL DEFAULT 'login', method TEXT NOT NULL DEFAULT '', created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE of_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, type TEXT, payload TEXT, status TEXT, max_attempts INTEGER)`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, password TEXT, role TEXT)`,
		totpEndpointsDDL, userTOTPDDL,
	)
	db.SetMaxOpenConns(1)
	h := magicTestDB(t)
	h.magicEmailLimiter = middleware.NewRateLimiter(0.05, 3)
	ae := &authEndpointConfig{TableName: "members", Path: "auth", UsernameColumn: "email", PasswordColumn: "password", RoleColumn: "role", DefaultRole: "user"}

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/magic/request", strings.NewReader(`{"email":"victim@x.io"}`))
		req.Host = "localhost:5733"
		rec := httptest.NewRecorder()
		h.magicRequest(rec, req, 1, db, db, "members", ae, 15, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	var jobs int
	db.QueryRow(`SELECT COUNT(*) FROM of_jobs`).Scan(&jobs)
	if jobs != 3 {
		t.Errorf("%d emails queued for one address from 10 requests, want the burst of 3", jobs)
	}
}

// TestMagicVerifyPublishesAuthEvents pins that a magic-link sign-in raises the same
// auth.register / auth.login events a password sign-in does, tagged with its method,
// so a welcome-email or last-login trigger does not silently skip these users.
func TestMagicVerifyPublishesAuthEvents(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE of_magic_tokens (token TEXT PRIMARY KEY, auth_endpoint_path TEXT NOT NULL, email TEXT NOT NULL, user_id INTEGER, expires_at DATETIME NOT NULL, used BOOLEAN DEFAULT 0, purpose TEXT NOT NULL DEFAULT 'login', method TEXT NOT NULL DEFAULT '', created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE of_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, type TEXT, payload TEXT, status TEXT, max_attempts INTEGER)`,
		`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, password TEXT, role TEXT)`,
		totpEndpointsDDL, userTOTPDDL,
	)
	db.SetMaxOpenConns(1)
	h := magicTestDB(t)
	bus := events.NewBus()
	h.deps.Bus = bus
	var got []events.Event
	bus.SubscribeAll(func(e events.Event) { got = append(got, e) })
	ae := &authEndpointConfig{TableName: "members", Path: "auth", UsernameColumn: "email", PasswordColumn: "password", RoleColumn: "role", DefaultRole: "user"}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic/request", strings.NewReader(`{"email":"new@x.io"}`))
	req.Host = "localhost:5733"
	h.magicRequest(httptest.NewRecorder(), req, 1, db, db, "members", ae, 15, "")
	var payload string
	if err := db.QueryRow(`SELECT payload FROM of_jobs`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	i := strings.Index(payload, "token=")
	if i < 0 {
		t.Fatalf("no token in queued email: %s", payload)
	}
	raw := payload[i+len("token=") : i+len("token=")+64]

	w := httptest.NewRecorder()
	h.magicVerify(w, httptest.NewRequest(http.MethodGet, "/api/auth/magic/verify?token="+raw, nil), 1, db, db, "members", ae, "")
	if w.Code != http.StatusOK {
		t.Fatalf("verify status %d: %s", w.Code, w.Body.String())
	}

	seen := map[events.EventType]events.Event{}
	for _, e := range got {
		seen[e.Type] = e
	}
	for _, typ := range []events.EventType{events.EventAuthRegister, events.EventAuthLogin} {
		e, ok := seen[typ]
		if !ok {
			t.Fatalf("magic-link first sign-in did not publish %s (got %v)", typ, got)
		}
		if e.Payload["method"] != authMethodMagicLink || e.Payload["email"] != "new@x.io" || e.Payload["user_id"] == nil {
			t.Errorf("%s payload = %v, want method=magic_link, email and user_id", typ, e.Payload)
		}
	}
}
