package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openfabrica/db/models"
	"openfabrica/security"
	"openfabrica/store"
)

// authoredMemberRequest builds a request carrying a member (non-admin) JWT for
// userID in the test project, the identity an authored endpoint scopes writes to and
// shares reads across.
func authoredMemberRequest(t *testing.T, jwt *security.JWTManager, method, body string) (*http.Request, int) {
	t.Helper()
	const userID = 5
	token, err := jwt.Generate(security.TokenRequest{ProjectID: 1, UserID: userID, Username: "u", Role: "member"})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/", nil)
	} else {
		r = httptest.NewRequest(method, "/", strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return r, userID
}

// TestBuildAPIRequestContextAuthoredSharesReadsScopesWrites is the core of the
// authored owner_scope: an authenticated member reading an authored endpoint gets
// NO owner predicate (every member reads all rows), while the same member on a
// write method DOES (a caller edits only their own rows). The identity is still
// resolved either way, so the insert stamp can attribute the author.
func TestBuildAPIRequestContextAuthoredSharesReadsScopesWrites(t *testing.T) {
	jwt := security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
	h := &Handler{deps: &Deps{JWTManager: jwt}}
	ep := &apiEndpoint{OwnerColumn: "user_id", OwnerScope: store.OwnerScopeAuthored, RequiresAuth: true}

	getReq, uid := authoredMemberRequest(t, jwt, "GET", "")
	rcGet, _, ok := h.buildAPIRequestContext(httptest.NewRecorder(), getReq, &models.Project{ID: 1}, ep)
	if !ok {
		t.Fatal("authored GET denied for an authenticated member")
	}
	if rcGet.OwnerFilter != "" {
		t.Errorf("authored GET should be shared (no owner filter); got %q", rcGet.OwnerFilter)
	}
	if rcGet.AuthUserID != uid {
		t.Errorf("authored GET should still resolve the identity; AuthUserID=%d want %d", rcGet.AuthUserID, uid)
	}

	putReq, _ := authoredMemberRequest(t, jwt, "PUT", `{"body":"x"}`)
	rcPut, _, ok := h.buildAPIRequestContext(httptest.NewRecorder(), putReq, &models.Project{ID: 1}, ep)
	if !ok {
		t.Fatal("authored PUT denied for an authenticated member")
	}
	if rcPut.OwnerFilter == "" {
		t.Error("authored PUT must stay owner-scoped (a caller edits only their own rows); got no owner filter")
	}
}

// TestBuildAPIRequestContextOwnerScopesReads is the contrast case: a plain owner
// endpoint (empty owner_scope) keeps the owner filter on GET, so the authored
// exemption above is specific to owner_scope=authored and does not leak into the
// private-per-user model.
func TestBuildAPIRequestContextOwnerScopesReads(t *testing.T) {
	jwt := security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
	h := &Handler{deps: &Deps{JWTManager: jwt}}
	ep := &apiEndpoint{OwnerColumn: "user_id", OwnerScope: "", RequiresAuth: true}

	getReq, _ := authoredMemberRequest(t, jwt, "GET", "")
	rc, _, ok := h.buildAPIRequestContext(httptest.NewRecorder(), getReq, &models.Project{ID: 1}, ep)
	if !ok {
		t.Fatal("owner GET denied for an authenticated member")
	}
	if rc.OwnerFilter == "" {
		t.Error("plain owner endpoint must scope reads to the caller; got no owner filter")
	}
}

// TestApiListAuthoredReturnsEveryAuthorsRows drives the whole read path: an
// authored endpoint over a table holding rows from two different authors returns
// BOTH to a member reading it, where a plain owner endpoint would return only the
// caller's own. This is the "you only see your own messages" failure a per-user
// scope causes on shared content, reproduced and then closed by authored.
func TestApiListAuthoredReturnsEveryAuthorsRows(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, channel_id INTEGER, user_id INTEGER, body TEXT)`,
		`INSERT INTO messages (id, channel_id, user_id, body) VALUES (1, 9, 5, 'mine'), (2, 9, 7, 'theirs')`)
	jwt := security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
	h := &Handler{deps: &Deps{JWTManager: jwt}}

	count := func(ep *apiEndpoint) int {
		t.Helper()
		req, _ := authoredMemberRequest(t, jwt, "GET", "")
		rc, _, ok := h.buildAPIRequestContext(httptest.NewRecorder(), req, &models.Project{ID: 1}, ep)
		if !ok {
			t.Fatal("GET denied")
		}
		rec := httptest.NewRecorder()
		h.apiList(rec, req, db, "messages", ep, rc)
		if rec.Code != http.StatusOK {
			t.Fatalf("apiList status=%d body=%s", rec.Code, rec.Body.String())
		}
		var env struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		return env.Count
	}

	authored := &apiEndpoint{OwnerColumn: "user_id", OwnerScope: store.OwnerScopeAuthored, RequiresAuth: true, Methods: []string{"GET"}}
	if n := count(authored); n != 2 {
		t.Errorf("authored list returned %d rows, want 2 (both authors' messages shared)", n)
	}
	owner := &apiEndpoint{OwnerColumn: "user_id", OwnerScope: "", RequiresAuth: true, Methods: []string{"GET"}}
	if n := count(owner); n != 1 {
		t.Errorf("owner list returned %d rows, want 1 (only the caller's own)", n)
	}
}

// TestApiUpdateAuthoredScopedToAuthor confirms authored writes stay private: user
// 5 cannot edit a row authored by user 7 (no row matches the owner predicate), but
// can edit their own.
func TestApiUpdateAuthoredScopedToAuthor(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, channel_id INTEGER, user_id INTEGER, body TEXT)`,
		`INSERT INTO messages (id, channel_id, user_id, body) VALUES (1, 9, 5, 'mine'), (2, 9, 7, 'theirs')`)
	jwt := security.NewJWTManager("test-jwt-secret-0123456789abcdef", time.Hour)
	h := &Handler{deps: &Deps{JWTManager: jwt}}
	ep := &apiEndpoint{OwnerColumn: "user_id", OwnerScope: store.OwnerScopeAuthored, RequiresAuth: true, Methods: []string{"PUT"}}

	update := func(rowID string) int {
		t.Helper()
		req, _ := authoredMemberRequest(t, jwt, "PUT", `{"body":"edited"}`)
		rc, _, ok := h.buildAPIRequestContext(httptest.NewRecorder(), req, &models.Project{ID: 1}, ep)
		if !ok {
			t.Fatal("PUT denied")
		}
		rec := httptest.NewRecorder()
		h.apiUpdate(rec, req, db, "messages", rowID, ep, 1, "messages", rc)
		return rec.Code
	}

	if code := update("2"); code != http.StatusNotFound {
		t.Errorf("member 5 editing author 7's row: status=%d, want 404 (write scoped to author)", code)
	}
	if code := update("1"); code != http.StatusOK {
		t.Errorf("member 5 editing their own row: status=%d, want 200", code)
	}
}
