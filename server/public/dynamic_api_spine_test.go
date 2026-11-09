package public

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"openfabrica/db"
	"openfabrica/db/models"

	_ "modernc.org/sqlite"
)

func spineDispatchDB(t *testing.T) (*db.ProjectDBManager, *db.ProjectDB) {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	return mgr, pdb
}

func mustExec(t *testing.T, w *sql.DB, q string) {
	t.Helper()
	if _, err := w.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func claimSpine(t *testing.T, w *sql.DB, path, sub, method, kind string) {
	t.Helper()
	if _, err := w.Exec("INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES (?,?,?,?)",
		path, sub, method, kind); err != nil {
		t.Fatalf("claim spine (%s/%s/%s=%s): %v", path, sub, method, kind, err)
	}
}

func spineReq(t *testing.T, h *Handler, method, url string) int {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	req = req.WithContext(context.WithValue(req.Context(), projectContextKey, &models.Project{ID: 1}))
	rec := httptest.NewRecorder()
	h.DynamicAPI(rec, req)
	return rec.Code
}

// TestDispatchViaSpine_MethodAwareCoLocation drives the full DynamicAPI handler
// over a co-located pair: a POST-only function and a GET CRUD endpoint share
// /api/files. The spine routes GET -> CRUD and POST -> function; both succeed.
// Dispatch keyed on path alone answers GET with 405, since the function owns it.
func TestDispatchViaSpine_MethodAwareCoLocation(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('items','{"name":"TEXT"}','{}')`)
	mustExec(t, w, `CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`)
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, public_read) VALUES ('files','items','["GET"]',1)`)
	claimSpine(t, w, "files", "", "GET", "crud")
	mustExec(t, w, `INSERT INTO of_function_endpoints (path, code, methods) VALUES ('files','function handler(req,ctx){ return { ok: true }; }','POST')`)
	claimSpine(t, w, "files", "", "POST", "function")

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	if code := spineReq(t, h, "GET", "/api/files"); code != http.StatusOK {
		t.Fatalf("GET /api/files (CRUD list) = %d, want 200", code)
	}
	if code := spineReq(t, h, "POST", "/api/files"); code != http.StatusOK {
		t.Fatalf("POST /api/files (function) = %d, want 200", code)
	}
}

// TestDispatchViaSpine_UploadSubRoute proves a reserved sub-route (/upload) is
// resolved from the spine to the upload handler, not misrouted to CRUD. apiUpload
// answers a body-less POST with 400, which distinguishes it from a 404.
func TestDispatchViaSpine_UploadSubRoute(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_upload_endpoints (path, allowed_types, max_size_mb, table_name) VALUES ('files','["image/*"]',5,'')`)
	claimSpine(t, w, "files", "upload", "POST", "upload")

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	if code := spineReq(t, h, "POST", "/api/files/upload"); code != http.StatusBadRequest {
		t.Fatalf("POST /api/files/upload = %d, want 400 (routed to apiUpload)", code)
	}
}

// TestDispatchViaSpine_WSRoutes drives a WebSocket connect THROUGH the spine
// dispatch (the WS relay tests mount apiWebSocket directly). The spine must
// resolve /api/<p>/ws to the WS handler (a non-upgrade GET fails the handshake
// with 400, distinct from the 404 apiWebSocket returns when the endpoint row is
// absent, and from the CRUD 404 when the ws claim is missing) and /api/<p>/rooms
// to apiWSRooms (200 room listing). The ?room= query must survive the last-slash
// path split.
func TestDispatchViaSpine_WSRoutes(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_ws_endpoints (path, requires_auth, tick_hz) VALUES ('world', 0, 20)`)
	claimSpine(t, w, "world", "ws", "*", "ws")
	claimSpine(t, w, "world", "rooms", "GET", "ws")

	h := &Handler{deps: &Deps{ProjectDBManager: mgr, WSHub: NewWSHub()}}

	// /ws routes to apiWebSocket; a plain GET (no Upgrade headers) fails the
	// handshake with 426 Upgrade Required - proving it reached the WS handler,
	// not CRUD (which would 404 on an unknown /world/<id>).
	if code := spineReq(t, h, "GET", "/api/world/ws?room=lobby"); code != http.StatusUpgradeRequired {
		t.Fatalf("GET /api/world/ws?room=lobby = %d, want 426 (routed to apiWebSocket, handshake fails)", code)
	}
	// /rooms routes to apiWSRooms, which lists the (empty) room and returns 200.
	if code := spineReq(t, h, "GET", "/api/world/rooms?room=lobby"); code != http.StatusOK {
		t.Fatalf("GET /api/world/rooms?room=lobby = %d, want 200 (routed to apiWSRooms)", code)
	}
}

// TestDispatchViaSpine_CrudSingleRowStillWorks confirms CRUD single-row routing
// survives the rewrite: a bare CRUD owner serves /api/<path>/<id> even though no
// spine row exists for the id segment (CRUD is handled after dispatchViaSpine).
func TestDispatchViaSpine_CrudSingleRowStillWorks(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('items','{"name":"TEXT"}','{}')`)
	mustExec(t, w, `CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`)
	mustExec(t, w, `INSERT INTO items (id, name) VALUES (1, 'a')`)
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, public_read) VALUES ('items','items','["GET"]',1)`)
	claimSpine(t, w, "items", "", "GET", "crud")

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	if code := spineReq(t, h, "GET", "/api/items/1"); code != http.StatusOK {
		t.Fatalf("GET /api/items/1 (CRUD single-row) = %d, want 200", code)
	}
}

// TestDispatchViaSpine_UndeclaredMethodIs405 pins the guide's claim ("a method no
// kind at the path declares returns 405") against the real route. 404 sends the
// model off to create the endpoint that already exists, which then collides - a
// two-step failure where 405 says the actual problem in one.
func TestDispatchViaSpine_UndeclaredMethodIs405(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_function_endpoints (path, code, methods) VALUES ('solo','function handler(req,ctx){ return { ok: true }; }','POST')`)
	claimSpine(t, w, "solo", "", "POST", "function")

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	if code := spineReq(t, h, "POST", "/api/solo"); code != http.StatusOK {
		t.Fatalf("POST /api/solo = %d, want 200", code)
	}
	if code := spineReq(t, h, "GET", "/api/solo"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/solo = %d, want 405 (the path exists; only the method is wrong)", code)
	}
}

// TestDispatchViaSpine_UnknownPathIs404 is the counterweight: 405 must not
// swallow a genuinely missing endpoint.
func TestDispatchViaSpine_UnknownPathIs404(t *testing.T) {
	mgr, _ := spineDispatchDB(t)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	if code := spineReq(t, h, "GET", "/api/nothing"); code != http.StatusNotFound {
		t.Fatalf("GET /api/nothing = %d, want 404", code)
	}
}

// A CRUD endpoint whose path contains a slash ("orders/recent", the example the tool
// text gives) has its LIST reached at /api/orders/recent. The router splits at the last
// slash, so with no endpoint at the prefix the whole path must still be tried before the
// request is answered 405/404.
func TestDynamicAPI_NestedCRUDPathListResolves(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('items','{"name":"TEXT"}','{}')`)
	mustExec(t, w, `CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`)
	mustExec(t, w, `INSERT INTO items (name) VALUES ('a')`)
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, public_read) VALUES ('orders/recent','items','["GET"]',1)`)
	claimSpine(t, w, "orders/recent", "", "GET", "crud")

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	if code := spineReq(t, h, "GET", "/api/orders/recent"); code != http.StatusOK {
		t.Fatalf("GET /api/orders/recent = %d, want 200 (the nested CRUD list)", code)
	}
	if code := spineReq(t, h, "GET", "/api/orders/recent/1"); code != http.StatusOK {
		t.Fatalf("GET /api/orders/recent/1 = %d, want 200 (single row)", code)
	}
}

// An endpoint registered at a nested path owns that URL even when its prefix is an
// endpoint too. Read as row "new" of a role-gated "orders", the public submission
// endpoint "orders/new" answered every anonymous POST with the parent's 401.
func TestDynamicAPI_NestedCRUDPathBeatsParentRow(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('items','{"name":"TEXT"}','{}')`)
	mustExec(t, w, `CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`)
	mustExec(t, w, `INSERT INTO items (name) VALUES ('a')`)
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, required_role) VALUES ('orders','items','["GET","POST"]',1,'admin')`)
	claimSpine(t, w, "orders", "", "GET", "crud")
	claimSpine(t, w, "orders", "", "POST", "crud")
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, public_read) VALUES ('orders/new','items','["GET"]',1)`)
	claimSpine(t, w, "orders/new", "", "GET", "crud")

	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	if code := spineReq(t, h, "GET", "/api/orders/new"); code != http.StatusOK {
		t.Fatalf("GET /api/orders/new = %d, want 200 from the endpoint registered there, not the parent's row route", code)
	}
	if code := spineReq(t, h, "GET", "/api/orders/1"); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/orders/1 = %d, want the parent's 401: a row of orders is still the parent's", code)
	}
}
