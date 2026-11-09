package public

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"openfabrica/db/models"
)

// corsPreflight drives a full OPTIONS request through DynamicAPI. coarse seeds
// the response headers the router's instance-wide CORS layer would have set
// before handing the preflight down (see middleware.CORSWithPreflightPassthrough),
// so a test can assert the per-endpoint policy overrides or keeps them.
func corsPreflight(t *testing.T, h *Handler, url, origin string, coarse map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodOptions, url, nil)
	req = req.WithContext(context.WithValue(req.Context(), projectContextKey, &models.Project{ID: 1}))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	for k, v := range coarse {
		rec.Header().Set(k, v)
	}
	h.DynamicAPI(rec, req)
	return rec
}

// permissiveCoarse is what the default instance-wide policy (empty cors_origins)
// puts on every response: Access-Control-Allow-Origin: * and no credentials.
var permissiveCoarse = map[string]string{"Access-Control-Allow-Origin": "*"}

// TestDynamicAPIPreflight_CRUDRestrictiveAllowlist verifies that a CRUD endpoint
// with a restrictive per-endpoint allowlist ENFORCES it on the preflight, even
// though the coarse instance-wide layer already put "*" on the response. A
// disallowed origin gets no Allow-Origin (the coarse "*" is dropped); an allowed
// origin is reflected with credentials so a credentialed cross-origin request
// succeeds. The per-endpoint allowlist must run on the preflight rather than the
// router answering it with the permissive "*".
func TestDynamicAPIPreflight_CRUDRestrictiveAllowlist(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, cors_origins) VALUES ('contacts','contacts','["GET","POST"]','https://good.com')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	// Disallowed origin: the coarse "*" must be removed, leaving no Allow-Origin.
	rec := corsPreflight(t, h, "/api/contacts", "https://evil.com", permissiveCoarse)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("disallowed preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed preflight Allow-Origin = %q, want \"\" (coarse * must be dropped)", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("disallowed preflight Allow-Credentials = %q, want \"\"", got)
	}

	// Allowed origin: reflected with credentials + Vary, per-endpoint methods.
	rec = corsPreflight(t, h, "/api/contacts", "https://good.com", permissiveCoarse)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://good.com" {
		t.Errorf("allowed preflight Allow-Origin = %q, want https://good.com", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("allowed preflight Allow-Credentials = %q, want true", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("allowed preflight Vary = %q, want Origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Errorf("allowed preflight Allow-Methods = %q, want \"GET, POST\"", got)
	}
}

// TestDynamicAPIPreflight_CRUDSingleRow proves the single-row URL
// (/api/<path>/<id>) resolves to the same CRUD endpoint's policy - the id segment
// has no spine row, so the resolver must fall back to the base path.
func TestDynamicAPIPreflight_CRUDSingleRow(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, cors_origins) VALUES ('contacts','contacts','["GET","PUT"]','https://good.com')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	rec := corsPreflight(t, h, "/api/contacts/42", "https://evil.com", permissiveCoarse)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("single-row disallowed preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("single-row disallowed Allow-Origin = %q, want \"\"", got)
	}
	rec = corsPreflight(t, h, "/api/contacts/42", "https://good.com", permissiveCoarse)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://good.com" {
		t.Errorf("single-row allowed Allow-Origin = %q, want https://good.com", got)
	}
}

// TestDynamicAPIPreflight_LLMSubRoute covers the LLM endpoint preflight: spine
// dispatch is method-gated (OPTIONS is never a stored method), so apiLLM's own
// OPTIONS branch never runs and the centralized resolver must find the policy by
// base path for both /chat and /complete.
func TestDynamicAPIPreflight_LLMSubRoute(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_llm_endpoints (path, cors_origins) VALUES ('assistant','https://good.com')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	rec := corsPreflight(t, h, "/api/assistant/chat", "https://evil.com", permissiveCoarse)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("llm /chat disallowed preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("llm /chat disallowed Allow-Origin = %q, want \"\"", got)
	}

	rec = corsPreflight(t, h, "/api/assistant/complete", "https://good.com", permissiveCoarse)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://good.com" {
		t.Errorf("llm /complete allowed Allow-Origin = %q, want https://good.com", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("llm /complete allowed Allow-Credentials = %q, want true", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "POST, OPTIONS" {
		t.Errorf("llm /complete allowed Allow-Methods = %q, want \"POST, OPTIONS\"", got)
	}
}

// TestDynamicAPIPreflight_Function covers the function endpoint preflight - the
// other kind whose OPTIONS branch was unreachable through the method-gated spine.
func TestDynamicAPIPreflight_Function(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_function_endpoints (path, code, methods, cors_origins) VALUES ('submit','function handler(req,ctx){ return { ok: true }; }','POST','https://good.com')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	rec := corsPreflight(t, h, "/api/submit", "https://evil.com", permissiveCoarse)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("function disallowed preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("function disallowed Allow-Origin = %q, want \"\"", got)
	}
	rec = corsPreflight(t, h, "/api/submit", "https://good.com", permissiveCoarse)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://good.com" {
		t.Errorf("function allowed Allow-Origin = %q, want https://good.com", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "POST, OPTIONS" {
		t.Errorf("function allowed Allow-Methods = %q, want \"POST, OPTIONS\"", got)
	}
}

// TestDynamicAPIPreflight_NoPerEndpointPolicyKeepsCoarse proves the fallback: an
// endpoint that declares no per-endpoint allowlist keeps the coarse instance-wide
// headers on preflight and still answers 204 (never the 405 the method dispatch
// would give for OPTIONS). This is the no-regression case for the permissive
// default and for endpoints that deliberately inherit the instance policy.
func TestDynamicAPIPreflight_NoPerEndpointPolicyKeepsCoarse(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, cors_origins) VALUES ('open','open','["GET"]','')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	rec := corsPreflight(t, h, "/api/open", "https://anywhere.com", permissiveCoarse)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("coarse-fallback preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("coarse-fallback Allow-Origin = %q, want * (coarse policy kept)", got)
	}
}

// TestDynamicAPIPreflight_WildcardPerEndpoint checks that a per-endpoint "*"
// reflects "*" and strips any credentials the coarse layer set for a matched
// instance-wide origin (the "* + credentials" pair is invalid).
func TestDynamicAPIPreflight_WildcardPerEndpoint(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, cors_origins) VALUES ('open','open','["GET"]','*')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	// Simulate a coarse layer that matched a real instance-wide allowlist and set
	// credentials; the per-endpoint "*" must clear them.
	rec := corsPreflight(t, h, "/api/open", "https://any.com", map[string]string{
		"Access-Control-Allow-Origin":      "https://any.com",
		"Access-Control-Allow-Credentials": "true",
	})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("wildcard Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("wildcard Allow-Credentials = %q, want \"\" (* + credentials is invalid)", got)
	}
}
