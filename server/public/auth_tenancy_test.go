package public

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"openfabrica/internal/stringutil"
	"openfabrica/security"
)

func tenancyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// :memory: gives each connection its own database; pin to one so the tx in
	// consumeInviteToken and the follow-up assertions see the same data.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE of_tenants (
			id INTEGER PRIMARY KEY AUTOINCREMENT, slug TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL, is_deleted BOOLEAN DEFAULT 0)`,
		`CREATE TABLE of_tenant_members (
			tenant_id INTEGER NOT NULL, user_id INTEGER NOT NULL,
			role TEXT NOT NULL DEFAULT 'member', status TEXT NOT NULL DEFAULT 'active',
			joined_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (tenant_id, user_id))`,
		`CREATE TABLE of_tenant_invites (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id INTEGER NOT NULL,
			email TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'member',
			token TEXT NOT NULL UNIQUE, expires_at DATETIME)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

// TestConsumeInviteToken_SingleUse: an invite is consumable exactly once. A
// replay must fail, and the row must be gone after the first use.
func TestConsumeInviteToken_SingleUse(t *testing.T) {
	db := tenancyDB(t)
	db.Exec(`INSERT INTO of_tenant_invites (tenant_id, email, role, token) VALUES (7, 'a@b.com', 'admin', 'inv1')`)

	h := &Handler{}
	tid, role, ok := h.consumeInviteToken(db, "inv1", "a@b.com")
	if !ok || tid != 7 || role != "admin" {
		t.Fatalf("first consume = (%d,%q,%v); want (7,admin,true)", tid, role, ok)
	}
	// Replay must fail.
	if _, _, ok := h.consumeInviteToken(db, "inv1", "a@b.com"); ok {
		t.Error("replayed invite token was accepted a second time")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM of_tenant_invites WHERE token = 'inv1'`).Scan(&n)
	if n != 0 {
		t.Errorf("invite row count = %d after consume; want 0 (deleted)", n)
	}
}

// TestConsumeInviteToken_Expired: an invite past expires_at must not be
// honoured, and must remain (not be consumed) so nothing changes.
func TestConsumeInviteToken_Expired(t *testing.T) {
	db := tenancyDB(t)
	db.Exec(`INSERT INTO of_tenant_invites (tenant_id, email, role, token, expires_at)
	         VALUES (7, 'a@b.com', 'member', 'expired', '2000-01-01 00:00:00')`)
	db.Exec(`INSERT INTO of_tenant_invites (tenant_id, email, role, token, expires_at)
	         VALUES (8, 'a@b.com', 'member', 'fresh', '2999-01-01 00:00:00')`)

	h := &Handler{}
	if _, _, ok := h.consumeInviteToken(db, "expired", "a@b.com"); ok {
		t.Error("expired invite was accepted")
	}
	if tid, _, ok := h.consumeInviteToken(db, "fresh", "a@b.com"); !ok || tid != 8 {
		t.Errorf("non-expired invite = (%d,_,%v); want (8,_,true)", tid, ok)
	}
}

// TestConsumeInviteToken_EmailScoping: a token bound to one email cannot be
// consumed by another; the empty-email (open) invite matches anyone.
func TestConsumeInviteToken_EmailScoping(t *testing.T) {
	db := tenancyDB(t)
	db.Exec(`INSERT INTO of_tenant_invites (tenant_id, email, role, token) VALUES (1, 'a@b.com', 'member', 'bound')`)
	db.Exec(`INSERT INTO of_tenant_invites (tenant_id, email, role, token) VALUES (2, '', 'member', 'open')`)

	h := &Handler{}
	if _, _, ok := h.consumeInviteToken(db, "bound", "intruder@x.com"); ok {
		t.Error("email-bound invite consumed by the wrong email")
	}
	if _, _, ok := h.consumeInviteToken(db, "bound", "a@b.com"); !ok {
		t.Error("email-bound invite rejected for the correct email")
	}
	if tid, _, ok := h.consumeInviteToken(db, "open", "anyone@x.com"); !ok || tid != 2 {
		t.Errorf("open invite = (%d,_,%v); want (2,_,true)", tid, ok)
	}
}

// TestLoadActiveMemberships_FiltersInactiveAndDeleted underpins tenant-switch
// authorization: a user can only switch to tenants this returns, so it must
// exclude inactive memberships and deleted tenants.
func TestLoadActiveMemberships_FiltersInactiveAndDeleted(t *testing.T) {
	db := tenancyDB(t)
	db.Exec(`INSERT INTO of_tenants (id, slug, name, is_deleted) VALUES (1,'acme','Acme',0)`)
	db.Exec(`INSERT INTO of_tenants (id, slug, name, is_deleted) VALUES (2,'gone','Gone',1)`) // deleted
	db.Exec(`INSERT INTO of_tenants (id, slug, name, is_deleted) VALUES (3,'beta','Beta',0)`)
	db.Exec(`INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (1, 5, 'owner', 'active')`)
	db.Exec(`INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (2, 5, 'member', 'active')`)   // active but tenant deleted
	db.Exec(`INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (3, 5, 'member', 'disabled')`) // membership disabled
	db.Exec(`INSERT INTO of_tenant_members (tenant_id, user_id, role, status) VALUES (1, 6, 'member', 'active')`)   // other user

	h := &Handler{}
	got, err := h.loadActiveMemberships(db, 5)
	if err != nil {
		t.Fatalf("loadActiveMemberships: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("memberships = %d; want 1 (only active membership to a live tenant)", len(got))
	}
	if got[0].TenantID != 1 || got[0].Slug != "acme" || got[0].Role != "owner" {
		t.Errorf("membership = %+v; want tenant 1 / acme / owner", got[0])
	}
}

func TestSlugifyForTenant(t *testing.T) {
	cases := map[string]string{
		"Acme Corp":     "acme-corp",
		" Hello_World ": "hello-world",
		"a@b.com":       "ab-com", // '@' dropped, '.' -> '-'
		"---":           "",
	}
	for in, want := range cases {
		if got := slugifyForTenant(in); got != want {
			t.Errorf("slugifyForTenant(%q) = %q; want %q", in, got, want)
		}
		// The manage_tenancy tool mints slugs for the same tables; both sides
		// must stay identical (they delegate to stringutil.Slug, this pins
		// against either side going local again).
		if got, shared := slugifyForTenant(in), stringutil.Slug(in, 63); got != shared {
			t.Errorf("slugifyForTenant(%q) = %q; diverged from stringutil.Slug = %q", in, got, shared)
		}
	}
}

func registerReq(email, invite string) *http.Request {
	target := "/api/auth/register"
	if invite != "" {
		target += "?invite=" + invite
	}
	return httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"email":"`+email+`","password":"pw-123456"}`))
}

// An invite-only endpoint must refuse a bad invite BEFORE creating the account,
// or every failed attempt leaves a user behind and usernames can be squatted.
func TestRegisterInviteOnlyLeavesNoAccountOnBadInvite(t *testing.T) {
	db := tenancyDB(t)
	if _, err := db.Exec(`CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, password TEXT, role TEXT DEFAULT 'user')`); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO of_tenants (slug, name) VALUES ('acme','Acme')`)
	db.Exec(`INSERT INTO of_tenant_invites (tenant_id, email, role, token) VALUES (1, 'new@x.io', 'member', 'good')`)
	db.Exec(`INSERT INTO of_tenant_invites (tenant_id, email, role, token, expires_at) VALUES (1, 'old@x.io', 'member', 'stale', datetime('now','-1 day'))`)

	h := &Handler{deps: &Deps{JWTManager: security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)}}
	ae := &authEndpointConfig{TableName: "members", Path: "auth", UsernameColumn: "email", PasswordColumn: "password", RoleColumn: "role", DefaultRole: "user", TenantSignupMode: "invite_only"}

	count := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM members`).Scan(&n)
		return n
	}
	for name, req := range map[string]*http.Request{
		"no invite":      registerReq("a@x.io", ""),
		"unknown invite": registerReq("a@x.io", "nope"),
		"expired invite": registerReq("old@x.io", "stale"),
		"wrong email":    registerReq("other@x.io", "good"),
	} {
		rec := httptest.NewRecorder()
		h.authRegister(rec, req, 1, db, db, "members", ae)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
		if n := count(); n != 0 {
			t.Fatalf("%s: left %d account(s) behind", name, n)
		}
	}

	rec := httptest.NewRecorder()
	h.authRegister(rec, registerReq("new@x.io", "good"), 1, db, db, "members", ae)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid invite: status %d (%s)", rec.Code, rec.Body.String())
	}
	if count() != 1 {
		t.Error("valid invite should create exactly one account")
	}
}
