package public

import (
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"
)

func createAPIEndpoint(t *testing.T, pdb *db.ProjectDB, path, tableName, methodsJSON string) {
	t.Helper()
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_api_endpoints (path, table_name, methods, public_read) VALUES (?,?,?,1)`,
		path, tableName, methodsJSON,
	); err != nil {
		t.Fatalf("insert api endpoint: %v", err)
	}
}

// TestFunctionFallsThroughToCoLocatedCRUD pins the method-aware co-location fix:
// a POST-only function and a GET CRUD endpoint can share /api/files. The
// function must claim POST, and on GET (which it doesn't declare) it must fall
// through so the co-located CRUD endpoint can serve the request - instead of
// swallowing the path and 405ing the GET.
func TestFunctionFallsThroughToCoLocatedCRUD(t *testing.T) {
	pdb := newFunctionTestDB(t,
		`CREATE TABLE files (id INTEGER PRIMARY KEY, slug TEXT, url TEXT)`)
	createFunction(t, pdb, "files", `function handler(req, ctx){ return { created: true }; }`, "POST", false, "")
	createAPIEndpoint(t, pdb, "files", "files", `["GET"]`)
	h := &Handler{deps: &Deps{}}

	// POST -> the function owns it and answers.
	reqPost := httptest.NewRequest("POST", "/api/files", strings.NewReader(`{}`))
	reqPost.Header.Set("Content-Type", "application/json")
	recPost := httptest.NewRecorder()
	if !h.serveFunctionIfExists(recPost, reqPost, &models.Project{ID: 1}, pdb, "files") {
		t.Fatal("POST: expected the function to handle the request")
	}
	if recPost.Code != 200 {
		t.Fatalf("POST status = %d, want 200 (%s)", recPost.Code, recPost.Body.String())
	}

	// GET -> the function doesn't declare it but a co-located CRUD GET does, so
	// serveFunctionIfExists must return false (fall through), NOT write a 405.
	reqGet := httptest.NewRequest("GET", "/api/files", nil)
	recGet := httptest.NewRecorder()
	if h.serveFunctionIfExists(recGet, reqGet, &models.Project{ID: 1}, pdb, "files") {
		t.Fatalf("GET: expected fall-through (false) to the co-located CRUD endpoint, but the function claimed it (status %d)", recGet.Code)
	}
}

// TestFunctionKeeps405WhenNoCoLocatedEndpoint pins serveFunctionIfExists in
// isolation: with no CRUD endpoint at the path, a method the function doesn't
// declare does not fall through (fall-through only happens when another kind
// actually handles the method).
//
// This calls the helper directly, so it does NOT describe what /api/solo returns
// - the spine gates entry, so a request only reaches here on a method the
// function declares, and the 405 below is unreachable through the route.
// TestDispatchViaSpine_UndeclaredMethodIs405 owns the route-level behaviour.
func TestFunctionKeeps405WhenNoCoLocatedEndpoint(t *testing.T) {
	pdb := newFunctionTestDB(t)
	createFunction(t, pdb, "solo", `function handler(req, ctx){ return { ok: true }; }`, "POST", false, "")
	h := &Handler{deps: &Deps{}}

	reqGet := httptest.NewRequest("GET", "/api/solo", nil)
	recGet := httptest.NewRecorder()
	if !h.serveFunctionIfExists(recGet, reqGet, &models.Project{ID: 1}, pdb, "solo") {
		t.Fatal("GET: expected the function to own the path (no co-located endpoint)")
	}
	if recGet.Code != 405 {
		t.Fatalf("GET status = %d, want 405", recGet.Code)
	}
}
