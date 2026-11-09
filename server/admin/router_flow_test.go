package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/firewall"
	"openfabrica/security"
	sdeps "openfabrica/server/deps"
)

// adminFlow drives the real admin route tree (public group, JWT guard, role
// groups) over a real database, the closest thing to a signed-in browser session
// that runs without a browser. It is how the owner-facing features added for
// launch are checked through their actual URLs, methods and permissions.
type adminFlow struct {
	t       *testing.T
	router  http.Handler
	dataDir string
	pm      *db.ProjectDBManager
}

const flowPassword = "Start-Pass-1!"

func newAdminFlow(t *testing.T) *adminFlow {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "openfabrica.db")
	gdb, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { gdb.Close() })
	pm := db.NewProjectDBManager(dataDir)
	t.Cleanup(func() { pm.CloseAll() })

	hash, err := security.HashPassword(flowPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := models.CreateUser(gdb.DB, "owner", hash, "admin", "Owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := models.CreateUser(gdb.DB, "viewer", hash, "user", "Viewer"); err != nil {
		t.Fatal(err)
	}

	deps := &Deps{Core: sdeps.Core{
		Config: &config.Config{
			AdminBindAddr: "127.0.0.1", AdminPort: 5734, PublicPort: 5733,
			DataDir: dataDir, DBPath: dbPath, PublicCSP: config.DefaultPublicCSP,
		},
		DB:               gdb,
		ProjectDBManager: pm,
		JWTManager:       security.NewJWTManager("flow-test-jwt-secret-at-least-32-bytes", time.Hour),
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:          "9.9.9",
		FirewallManager:  &fakeFirewall{rulePresent: true},
	}}
	r := chi.NewRouter()
	RegisterRoutes(r, deps, nil)
	return &adminFlow{t: t, router: r, dataDir: dataDir, pm: pm}
}

func (f *adminFlow) do(method, path, token, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *adminFlow) login(username, password string) string {
	f.t.Helper()
	b, _ := json.Marshal(loginRequest{Username: username, Password: password})
	rec := f.do(http.MethodPost, "/admin/api/auth/login", "", string(b))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("login %s: status %d, body %s", username, rec.Code, rec.Body.String())
	}
	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		f.t.Fatalf("login %s: no token: %v", username, err)
	}
	return resp.Token
}

// The new operator pages are reachable by their real URLs for an admin, refuse an
// anonymous caller, and are closed to a non-admin account.
func TestAdminOnlySystemPagesThroughTheRouter(t *testing.T) {
	f := newAdminFlow(t)
	admin := f.login("owner", flowPassword)
	viewer := f.login("viewer", flowPassword)

	for _, path := range []string{"/admin/api/system/config", "/admin/api/system/logs", "/admin/api/system/backup"} {
		if rec := f.do(http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous: status %d, want 401", path, rec.Code)
		}
		if rec := f.do(http.MethodGet, path, viewer, ""); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as a non-admin: status %d, want 403", path, rec.Code)
		}
		if rec := f.do(http.MethodGet, path, admin, ""); rec.Code != http.StatusOK {
			t.Errorf("GET %s as admin: status %d, want 200 (body %.200s)", path, rec.Code, rec.Body.String())
		}
	}

	// The backup an admin downloads is a real archive holding the core database.
	rec := f.do(http.MethodGet, "/admin/api/system/backup", admin, "")
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("backup is not a zip: %v", err)
	}
	found := false
	for _, e := range zr.File {
		found = found || e.Name == "openfabrica.db"
	}
	if !found {
		t.Error("backup archive lacks openfabrica.db")
	}

	// The config view never carries a key, and says the admin is local-only here.
	cfg := f.do(http.MethodGet, "/admin/api/system/config", admin, "").Body.String()
	if strings.Contains(cfg, "jwt") || strings.Contains(cfg, "encryption") {
		t.Errorf("config view mentions a key: %s", cfg)
	}
	if !strings.Contains(cfg, `"admin_exposed":false`) {
		t.Errorf("a loopback-bound admin reported as exposed: %s", cfg)
	}
}

// Every signed-in account, whatever its role, may change its own password; doing so
// ends the other sessions and the response carries a token that keeps this one.
func TestChangePasswordThroughTheRouter(t *testing.T) {
	f := newAdminFlow(t)
	for _, name := range []string{"owner", "viewer"} {
		old := f.login(name, flowPassword)
		const next = "Fresh-Pass-2@"

		wrong, _ := json.Marshal(changePasswordRequest{CurrentPassword: "nope", NewPassword: next})
		if rec := f.do(http.MethodPut, "/admin/api/auth/password", old, string(wrong)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: wrong current password: status %d, want 400", name, rec.Code)
		}
		if rec := f.do(http.MethodPut, "/admin/api/auth/password", "", string(wrong)); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous change: status %d, want 401", name, rec.Code)
		}

		good, _ := json.Marshal(changePasswordRequest{CurrentPassword: flowPassword, NewPassword: next})
		rec := f.do(http.MethodPut, "/admin/api/auth/password", old, string(good))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: change password: status %d, body %s", name, rec.Code, rec.Body.String())
		}
		var resp loginResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" {
			t.Fatalf("%s: response lacks a token", name)
		}

		// Any page the account may open: the old token is dead, the new one works.
		const page = "/admin/api/auth/revoke-sessions"
		if rec := f.do(http.MethodPost, page, old, "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: old token after the change: status %d, want 401", name, rec.Code)
		}
		if rec := f.do(http.MethodPost, page, resp.Token, "{}"); rec.Code != http.StatusOK {
			t.Errorf("%s: new token after the change: status %d, want 200", name, rec.Code)
		}
		if got := f.login(name, next); got == "" {
			t.Errorf("%s: cannot sign in with the new password", name)
		}
	}
}

// "Sign out other devices" kills every token minted before it and hands back one
// that still works.
func TestRevokeSessionsThroughTheRouter(t *testing.T) {
	f := newAdminFlow(t)
	laptop := f.login("owner", flowPassword)
	phone := f.login("owner", flowPassword)

	rec := f.do(http.MethodPost, "/admin/api/auth/revoke-sessions", laptop, "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if got := f.do(http.MethodGet, "/admin/api/system/config", phone, "").Code; got != http.StatusUnauthorized {
		t.Errorf("the other device's token: status %d, want 401", got)
	}
	if got := f.do(http.MethodGet, "/admin/api/system/config", laptop, "").Code; got != http.StatusUnauthorized {
		t.Errorf("the pre-revoke token of this device: status %d, want 401 (it must use the replacement)", got)
	}
	if got := f.do(http.MethodGet, "/admin/api/system/config", resp.Token, "").Code; got != http.StatusOK {
		t.Errorf("the replacement token: status %d, want 200", got)
	}
}

// An exposed instance with no admin account yet prints a setup token to its log and
// refuses every first-run call without it; a loopback instance asks for nothing.
func TestSetupTokenThroughTheRouter(t *testing.T) {
	setup := func(bind string) (http.Handler, *bytes.Buffer) {
		gdb, err := db.Open(filepath.Join(t.TempDir(), "global.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { gdb.Close() })
		logs := &bytes.Buffer{}
		deps := &Deps{Core: sdeps.Core{
			Config:     &config.Config{AdminBindAddr: bind},
			DB:         gdb,
			JWTManager: security.NewJWTManager("flow-test-jwt-secret-at-least-32-bytes", time.Hour),
			Logger:     slog.New(slog.NewTextHandler(logs, nil)),
		}}
		r := chi.NewRouter()
		RegisterRoutes(r, deps, nil)
		return r, logs
	}
	send := func(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.9:50000"
		if token != "" {
			req.Header.Set("X-Setup-Token", token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	exposed, logs := setup("0.0.0.0")
	m := regexp.MustCompile(`setup_token=(\w+)`).FindStringSubmatch(logs.String())
	if m == nil {
		t.Fatalf("no setup token was logged: %s", logs.String())
	}
	token := m[1]

	if got := send(exposed, http.MethodGet, "/admin/api/setup/check", "").Body.String(); !strings.Contains(got, `"needs_setup_token":true`) {
		t.Errorf("setup/check does not announce the token: %s", got)
	}
	for _, p := range []struct{ method, path string }{
		{http.MethodPost, "/admin/api/setup"},
		{http.MethodGet, "/admin/api/setup/providers"},
		{http.MethodPost, "/admin/api/setup/providers/1/sync"},
		{http.MethodPost, "/admin/api/setup/token"},
	} {
		if rec := send(exposed, p.method, p.path, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without the token: status %d, want 403", p.method, p.path, rec.Code)
		}
		if rec := send(exposed, p.method, p.path, "guess"); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a wrong token: status %d, want 403", p.method, p.path, rec.Code)
		}
	}
	if rec := send(exposed, http.MethodPost, "/admin/api/setup/token", token); rec.Code != http.StatusOK {
		t.Errorf("the logged token was refused: status %d", rec.Code)
	}

	local, localLogs := setup("127.0.0.1")
	if strings.Contains(localLogs.String(), "setup_token") {
		t.Error("a loopback instance logged a setup token")
	}
	if got := send(local, http.MethodGet, "/admin/api/setup/check", "").Body.String(); strings.Contains(got, `"needs_setup_token":true`) {
		t.Errorf("a loopback instance asks for a token: %s", got)
	}
	if rec := send(local, http.MethodPost, "/admin/api/setup/token", ""); rec.Code != http.StatusOK {
		t.Errorf("loopback token check: status %d, want 200", rec.Code)
	}
}

// Restoring an older version is an admin action; for a project that does not exist
// the answer is 404 and no project directory is created as a side effect.
func TestVersionRestorePermissionsThroughTheRouter(t *testing.T) {
	f := newAdminFlow(t)
	admin := f.login("owner", flowPassword)
	viewer := f.login("viewer", flowPassword)
	const path = "/admin/api/projects/7/files/1/restore"

	if rec := f.do(http.MethodPost, path, viewer, `{"version":1}`); rec.Code != http.StatusForbidden {
		t.Errorf("restore as a non-admin: status %d, want 403", rec.Code)
	}
	if rec := f.do(http.MethodPost, path, admin, `{"version":1}`); rec.Code != http.StatusNotFound {
		t.Errorf("restore in a project that does not exist: status %d, want 404", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "projects", "7")); err == nil {
		t.Error("a restore against a missing project created its directory")
	}
}

// fakeFirewall stands in for the operating system's firewall: it records what was
// asked and answers like a machine whose rule can be added and removed.
type fakeFirewall struct {
	rulePresent bool
	removed     int
	allowed     int
}

func (f *fakeFirewall) Detect(_ context.Context, _ firewall.Rule) (firewall.Status, error) {
	return firewall.Status{Supported: true, Active: true, RulePresent: f.rulePresent}, nil
}
func (f *fakeFirewall) Ensure(_ context.Context, _ firewall.Rule) (firewall.Result, error) {
	f.allowed++
	f.rulePresent = true
	return firewall.Result{OK: true}, nil
}
func (f *fakeFirewall) Remove(_ context.Context, name string) (firewall.Result, error) {
	f.removed++
	f.rulePresent = false
	return firewall.Result{OK: name == firewall.RuleName}, nil
}

// Anyone signed in may read the firewall state, but adding or removing the rule is
// an admin action, and removing it asks for Open Fabrica's own rule by name.
func TestFirewallRuleChangesAreAdminOnly(t *testing.T) {
	f := newAdminFlow(t)
	admin := f.login("owner", flowPassword)
	viewer := f.login("viewer", flowPassword)

	if rec := f.do(http.MethodGet, "/admin/api/firewall/status", viewer, ""); rec.Code != http.StatusOK {
		t.Errorf("status as a non-admin: %d, want 200", rec.Code)
	}
	for _, action := range []string{"allow", "remove"} {
		path := "/admin/api/firewall/" + action
		if rec := f.do(http.MethodPost, path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d, want 401", action, rec.Code)
		}
		if rec := f.do(http.MethodPost, path, viewer, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s as a non-admin: %d, want 403", action, rec.Code)
		}
	}

	rec := f.do(http.MethodPost, "/admin/api/firewall/remove", admin, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("remove as admin: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.do(http.MethodGet, "/admin/api/firewall/status", admin, "").Body.String(); !strings.Contains(got, `"rule_present":false`) {
		t.Errorf("the card would still show the rule after removing it: %s", got)
	}
	rec = f.do(http.MethodPost, "/admin/api/firewall/allow", admin, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("allow as admin: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.do(http.MethodGet, "/admin/api/firewall/status", admin, "").Body.String(); !strings.Contains(got, `"rule_present":true`) {
		t.Errorf("the card would not show the rule after adding it: %s", got)
	}
}
