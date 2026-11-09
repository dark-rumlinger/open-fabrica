package public

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/internal/tokenrevoke"
	"openfabrica/security"
)

// revocationFixture is a real, migrated project database with an account table
// ("members", the auth endpoint's table) holding two people, plus a handler that
// validates tokens against it. Project id 1 is what spineDispatchDB creates.
func revocationFixture(t *testing.T) (*Handler, *db.ProjectDB) {
	t.Helper()
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `CREATE TABLE members (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT UNIQUE, password TEXT, role TEXT, display_name TEXT, banned INTEGER DEFAULT 0)`)
	mustExec(t, w, `INSERT INTO members (email, password, role, display_name) VALUES ('ada@x.io', 'hash', 'user', 'Ada'), ('bob@x.io', 'hash', 'user', 'Bob')`)
	mustExec(t, w, `INSERT INTO of_auth_endpoints (table_name, path) VALUES ('members', 'auth')`)
	t.Cleanup(func() { tokenrevoke.Forget(1) })
	h := &Handler{deps: &Deps{
		ProjectDBManager: mgr,
		JWTManager:       security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour),
	}}
	return h, pdb
}

func (h *Handler) mint(t *testing.T, userID int) string {
	t.Helper()
	tok, err := h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: 1, UserID: userID, Username: "u", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// valid reports whether a token is accepted by every path the public server uses
// to accept a user's token: the claims check, the Authorization-header extractor and
// the combined API-key-or-JWT check the WebSocket and stream endpoints use.
func (h *Handler) valid(t *testing.T, tok string) bool {
	t.Helper()
	_, errClaims := h.validateUserClaims(tok)
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	_, errHeader := h.extractUserClaims(req)
	okCombined, _ := h.validateProjectTokenOrJWTWithClaims(1, tok)

	ok := errClaims == nil
	if (errHeader == nil) != ok || okCombined != ok {
		t.Fatalf("the acceptance paths disagree: claims=%v header=%v combined=%v", errClaims, errHeader, okCombined)
	}
	return ok
}

// The central check ends a user's older tokens and nobody else's, and a token the
// same user is given afterwards works.
func TestRevokedUserTokensAreRefusedOnEveryPath(t *testing.T) {
	h, pdb := revocationFixture(t)
	ada, bob := h.mint(t, 1), h.mint(t, 2)
	if !h.valid(t, ada) || !h.valid(t, bob) {
		t.Fatal("fresh tokens were refused")
	}

	time.Sleep(3 * time.Millisecond) // the next token must be issued after the revocation
	if err := tokenrevoke.Revoke(pdb.Writer(), 1, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)

	if h.valid(t, ada) {
		t.Error("Ada's token issued before the revocation still works")
	}
	if !h.valid(t, bob) {
		t.Error("revoking Ada ended Bob's session")
	}
	if !h.valid(t, h.mint(t, 1)) {
		t.Error("a token issued to Ada after the revocation was refused")
	}
}

// A revocation is stored, so a restarted server still refuses the old token.
func TestRevocationSurvivesAServerRestart(t *testing.T) {
	h, pdb := revocationFixture(t)
	old := h.mint(t, 1)
	time.Sleep(3 * time.Millisecond)
	if err := tokenrevoke.Revoke(pdb.Writer(), 1, 1); err != nil {
		t.Fatal(err)
	}
	tokenrevoke.Forget(1) // what a fresh process starts with: nothing cached
	if h.valid(t, old) {
		t.Error("a revocation was forgotten along with the in-memory cache")
	}
}

// A token of a project that no longer exists is not made valid by a failed lookup,
// and checking it does not create the project's directory.
func TestUserTokenForAMissingProjectCreatesNothing(t *testing.T) {
	h, _ := revocationFixture(t)
	tok, _ := h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: 99, UserID: 1, Username: "u", Role: "user"})
	if _, err := h.validateUserClaims(tok); err != nil {
		t.Fatalf("a signed token for another project was refused for a reason other than that project: %v", err)
	}
	if h.deps.ProjectDBManager.Exists(99) {
		t.Error("checking a token created a project database")
	}
}

func accountRevoked(t *testing.T, h *Handler, pdb *db.ProjectDB, tok string) bool {
	t.Helper()
	_, err := h.validateUserClaims(tok)
	return err != nil
}

// Changing what an account may do, or deleting it, ends its sessions through the
// app's own API; editing a display name does not.
func TestAccountChangesThroughTheAPIEndSessions(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		body    string
		revoked bool
	}{
		{"display name", http.MethodPut, `{"display_name":"Ada L."}`, false},
		{"password", http.MethodPut, `{"password":"new-hash"}`, true},
		{"role", http.MethodPut, `{"role":"admin"}`, true},
		{"banned flag", http.MethodPut, `{"banned":1}`, true},
		{"delete", http.MethodDelete, ``, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, pdb := revocationFixture(t)
			ep := &apiEndpoint{SecureCols: map[string]string{}}
			rc := &apiRequestContext{}
			tok := h.mint(t, 1)
			time.Sleep(3 * time.Millisecond)

			rec := httptest.NewRecorder()
			if c.method == http.MethodDelete {
				h.apiDelete(rec, pdb.Writer(), "members", "1", ep, 1, "members", rc)
			} else {
				req := httptest.NewRequest(c.method, "/api/members/1", strings.NewReader(c.body))
				h.apiUpdate(rec, req, pdb.Writer(), "members", "1", ep, 1, "members", rc)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d, body %s", c.name, rec.Code, rec.Body.String())
			}
			time.Sleep(3 * time.Millisecond)

			if got := accountRevoked(t, h, pdb, tok); got != c.revoked {
				t.Errorf("%s: token revoked = %v, want %v", c.name, got, c.revoked)
			}
			if bob := h.mint(t, 2); accountRevoked(t, h, pdb, bob) {
				t.Errorf("%s: another account's session ended", c.name)
			}
		})
	}
}

// A table that is not an auth endpoint's account table never revokes anything,
// whatever its columns are called.
func TestEditingAnOrdinaryTableEndsNoSessions(t *testing.T) {
	h, pdb := revocationFixture(t)
	mustExec(t, pdb.Writer(), `CREATE TABLE notes (id INTEGER PRIMARY KEY, role TEXT, password TEXT, body TEXT)`)
	mustExec(t, pdb.Writer(), `INSERT INTO notes (role, password, body) VALUES ('x', 'y', 'z')`)
	tok := h.mint(t, 1)
	time.Sleep(3 * time.Millisecond)

	req := httptest.NewRequest(http.MethodPut, "/api/notes/1", strings.NewReader(`{"role":"admin","password":"p"}`))
	rec := httptest.NewRecorder()
	h.apiUpdate(rec, req, pdb.Writer(), "notes", "1", &apiEndpoint{SecureCols: map[string]string{}}, 1, "notes", &apiRequestContext{})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d", rec.Code)
	}
	time.Sleep(3 * time.Millisecond)
	if accountRevoked(t, h, pdb, tok) {
		t.Error("editing an ordinary table ended someone's session")
	}
}

// A completed password reset ends the sessions opened with the old password.
func TestPasswordResetEndsExistingSessions(t *testing.T) {
	h, db, ae := resetFixture(t)
	if _, err := db.Exec(`CREATE TABLE of_token_revocations (user_id INTEGER PRIMARY KEY, revoked_before INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tokenrevoke.Forget(1) })

	if code := forgot(t, h, db, ae, "ada@x.io", "localhost:5733"); code != http.StatusOK {
		t.Fatalf("forgot: %d", code)
	}
	link := queuedLink(t, db)
	issuedBefore := time.Now()
	time.Sleep(3 * time.Millisecond)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/password/reset", strings.NewReader(`{"token":"`+link+`","password":"new-password"}`))
	rec := httptest.NewRecorder()
	h.handlePassword(rec, req, 1, db, db, "members", ae, "reset")
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: %d", rec.Code)
	}
	time.Sleep(3 * time.Millisecond)

	if !tokenrevoke.Revoked(db, 1, 1, &issuedBefore) {
		t.Error("a session opened before the reset survived it")
	}
	after := time.Now()
	if tokenrevoke.Revoked(db, 1, 1, &after) {
		t.Error("signing in after the reset was refused")
	}
}
