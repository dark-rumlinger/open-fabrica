package public

import (
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/db/models"
)

// TestApiUpdateRejectsOwnerTenantReassignment covers the owner/tenant reassignment regression: a scoped
// user (owner_id=1, tenant_id=1) who PUTs a body trying to reassign owner_id and
// tenant_id must update only the non-scope fields. The owner/tenant stay put, so
// the row cannot be handed to another user or injected into another tenant.
func TestApiUpdateRejectsOwnerTenantReassignment(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT, owner_id INTEGER, tenant_id INTEGER)`,
		`INSERT INTO notes (id, body, owner_id, tenant_id) VALUES (1, 'orig', 1, 1)`)

	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{OwnerColumn: "owner_id", TenantColumn: "tenant_id"}
	rc := &apiRequestContext{
		OwnerFilter: ` AND "owner_id" = ?`, OwnerFilterVal: 1, AuthUserID: 1,
		TenantFilter: ` AND "tenant_id" = ?`, TenantFilterVal: 1, AuthTenantID: 1,
	}

	req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"body":"updated","owner_id":777,"tenant_id":999}`))
	rec := httptest.NewRecorder()
	h.apiUpdate(rec, req, db, "notes", "1", ep, 1, "notes", rc)

	if rec.Code != 200 {
		t.Fatalf("apiUpdate status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var owner, tenant int
	var bodyText string
	if err := db.QueryRow(`SELECT owner_id, tenant_id, body FROM notes WHERE id = 1`).Scan(&owner, &tenant, &bodyText); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if owner != 1 {
		t.Errorf("owner_id reassigned to %d via PUT; want 1 (unchanged)", owner)
	}
	if tenant != 1 {
		t.Errorf("tenant_id reassigned to %d via PUT; want 1 (cross-tenant injection)", tenant)
	}
	if bodyText != "updated" {
		t.Errorf("body = %q, want \"updated\" (non-scope field should still update)", bodyText)
	}
}

// TestApiUpdateProjectKeyMayAssignOwner confirms the strip is identity-scoped:
// a project API key (no user/tenant identity, owner-level access) is still
// allowed to set owner_id/tenant_id, mirroring apiInsert's stamping condition.
func TestApiUpdateProjectKeyMayAssignOwner(t *testing.T) {
	db := setupRowIDColumnDB(t,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT, owner_id INTEGER, tenant_id INTEGER)`,
		`INSERT INTO notes (id, body, owner_id, tenant_id) VALUES (1, 'orig', 1, 1)`)

	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{OwnerColumn: "owner_id", TenantColumn: "tenant_id"}
	// Project key: no owner/tenant filter, no identity (AuthUserID/AuthTenantID 0).
	rc := &apiRequestContext{}

	req := httptest.NewRequest("PUT", "/", strings.NewReader(`{"body":"x","owner_id":777}`))
	rec := httptest.NewRecorder()
	h.apiUpdate(rec, req, db, "notes", "1", ep, 1, "notes", rc)

	if rec.Code != 200 {
		t.Fatalf("apiUpdate status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var owner int
	if err := db.QueryRow(`SELECT owner_id FROM notes WHERE id = 1`).Scan(&owner); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if owner != 777 {
		t.Errorf("project key could not assign owner_id; got %d, want 777", owner)
	}
}

// TestBuildAPIRequestContextScopedAnonymousDenied covers the scoped-anonymous-access regression: an
// owner-scoped endpoint that is public (requires_auth=false) but not public_read
// must NOT serve an anonymous caller, or it would return every owner's rows.
func TestBuildAPIRequestContextScopedAnonymousDenied(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{OwnerColumn: "owner_id", RequiresAuth: false, PublicRead: false}

	req := httptest.NewRequest("GET", "/", nil) // no Authorization header
	rec := httptest.NewRecorder()
	_, _, ok := h.buildAPIRequestContext(rec, req, &models.Project{ID: 1}, ep)

	if ok {
		t.Fatal("scoped endpoint served an anonymous request; expected denial")
	}
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401 (authentication required)", rec.Code)
	}
}

// TestBuildAPIRequestContextPublicReadAllowsAnonymous confirms the exemption:
// a public_read owner-scoped endpoint still serves anonymous reads (the blog
// pattern), returning a context with no owner filter rather than a denial.
func TestBuildAPIRequestContextPublicReadAllowsAnonymous(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{OwnerColumn: "owner_id", RequiresAuth: false, PublicRead: true}

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	rc, _, ok := h.buildAPIRequestContext(rec, req, &models.Project{ID: 1}, ep)

	if !ok {
		t.Fatalf("public_read endpoint denied an anonymous read (status %d)", rec.Code)
	}
	if rc.OwnerFilter != "" {
		t.Errorf("public_read read should be unscoped; got owner filter %q", rc.OwnerFilter)
	}
}

// TestBuildAPIRequestContextPublicWriteAllowsAnonymousPost is the public-write
// (contact-form) exemption: an auth-gated endpoint with public_write must let an
// anonymous POST through, the inverse of public_read. This is what a contact-form
// messages endpoint needs and plain requires_auth could not express.
func TestBuildAPIRequestContextPublicWriteAllowsAnonymousPost(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{RequiresAuth: true, PublicWrite: true, PublicRead: false}

	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"a"}`)) // no Authorization header
	rec := httptest.NewRecorder()
	_, _, ok := h.buildAPIRequestContext(rec, req, &models.Project{ID: 1}, ep)

	if !ok {
		t.Fatalf("public_write endpoint denied an anonymous POST (status %d)", rec.Code)
	}
}

// TestBuildAPIRequestContextPublicWriteGatesRead confirms the asymmetry: the same
// public_write endpoint (without public_read) still requires a token for GET, so
// only an authed admin reads submissions - no world-readable leak.
func TestBuildAPIRequestContextPublicWriteGatesRead(t *testing.T) {
	h := &Handler{deps: &Deps{}}
	ep := &apiEndpoint{RequiresAuth: true, PublicWrite: true, PublicRead: false}

	req := httptest.NewRequest("GET", "/", nil) // no Authorization header
	rec := httptest.NewRecorder()
	_, _, ok := h.buildAPIRequestContext(rec, req, &models.Project{ID: 1}, ep)

	if ok {
		t.Fatal("public_write endpoint served an anonymous GET; reads must stay gated")
	}
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401 (GET requires auth)", rec.Code)
	}
}
