package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// The APIs tab lists every kind of endpoint a project can have, labels each, and only
// lets the data APIs be deleted: the other kinds are read-only here, and a delete aimed
// at one must say it did nothing instead of reporting a removal.
func TestEndpointsListCoversEveryKindThroughTheRouter(t *testing.T) {
	f := newAdminFlow(t)
	admin := f.login("owner", flowPassword)
	pdb, err := f.pm.Create(7)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO of_api_endpoints (path, table_name, methods) VALUES ('posts', 'posts', '["GET","POST"]')`,
		`INSERT INTO of_llm_endpoints (path) VALUES ('assistant')`,
		`INSERT INTO of_function_endpoints (path, methods, timeout_ms) VALUES ('rates', 'get, post', 5000)`,
		`INSERT INTO of_agent_endpoints (path, max_steps) VALUES ('helper', 6)`,
		`INSERT INTO of_document_endpoints (path, template_html, landscape) VALUES ('invoice', '<p>x</p>', 1)`,
		`INSERT INTO of_export_endpoints (path, table_name, format) VALUES ('orders', 'orders', 'csv')`,
		`INSERT INTO of_import_endpoints (path, table_name, mode) VALUES ('orders', 'orders', 'upsert')`,
		`INSERT INTO of_query_endpoints (path, max_rows) VALUES ('_query', 500)`,
		`INSERT INTO of_feed_endpoints (path, table_name, format) VALUES ('news', 'posts', 'rss')`,
		`INSERT INTO of_push_endpoints (path, vapid_public_key, vapid_private_secret_name) VALUES ('push', 'k', 's')`,
		`INSERT INTO of_totp_endpoints (path, auth_endpoint_path, enforced) VALUES ('auth', 'auth', 1)`,
		`INSERT INTO of_magic_link_endpoints (path, auth_endpoint_path, ttl_minutes) VALUES ('auth', 'auth', 20)`,
	} {
		if _, err := pdb.ExecWrite(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	rec := f.do(http.MethodGet, "/admin/api/projects/7/endpoints", admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var eps []projectEndpoint
	if err := json.Unmarshal(rec.Body.Bytes(), &eps); err != nil {
		t.Fatal(err)
	}
	byKind := map[string]projectEndpoint{}
	ids := map[int]string{}
	for _, e := range eps {
		if prev, dup := ids[e.ID]; dup {
			t.Errorf("id %d is used by both %s and %s/%s", e.ID, prev, e.Kind, e.Path)
		}
		ids[e.ID] = e.Kind + "/" + e.Path
		byKind[e.Kind] = e
	}
	for _, kind := range []string{"api", "llm", "function", "agent", "document", "export", "import", "query", "feed", "push", "totp", "magic_link"} {
		if _, ok := byKind[kind]; !ok {
			t.Errorf("no %q endpoint in the list: %+v", kind, eps)
		}
	}

	check := func(kind, wantMethods, wantDetail string, wantAuth bool) {
		t.Helper()
		e := byKind[kind]
		if e.Methods != wantMethods {
			t.Errorf("%s methods = %s, want %s", kind, e.Methods, wantMethods)
		}
		if wantDetail != "" && e.Detail != wantDetail {
			t.Errorf("%s detail = %q, want %q", kind, e.Detail, wantDetail)
		}
		if e.RequiresAuth != wantAuth {
			t.Errorf("%s requires_auth = %v, want %v", kind, e.RequiresAuth, wantAuth)
		}
	}
	check("function", `["GET","POST"]`, "timeout 5000 ms", false)
	check("agent", `["POST"]`, "up to 6 steps", true)
	check("document", `["POST"]`, "A4 landscape", true)
	check("export", `["GET"]`, "CSV of orders", false)
	check("import", `["POST"]`, "upsert into orders", true)
	check("query", `["POST"]`, "read-only, up to 500 rows", false)
	check("feed", `["GET"]`, "RSS of posts", false)
	check("totp", `["POST"]`, "on login API auth (required)", true)
	check("magic_link", `["GET","POST"]`, "on login API auth, link lasts 20 min", false)

	// Only a data API can be deleted here. The others answer 404 and stay.
	if rec := f.do(http.MethodDelete, "/admin/api/projects/7/endpoints/"+strconv.Itoa(byKind["function"].ID), admin, ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleting a function endpoint through the API tab: %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/admin/api/projects/7/endpoints/"+strconv.Itoa(byKind["llm"].ID), admin, ""); rec.Code != http.StatusNotFound {
		t.Errorf("deleting an LLM endpoint through the API tab: %d, want 404", rec.Code)
	}
	var fn int
	if err := pdb.QueryRow(`SELECT COUNT(*) FROM of_function_endpoints`).Scan(&fn); err != nil || fn != 1 {
		t.Errorf("the function endpoint is gone after a refused delete: %d %v", fn, err)
	}
	if rec := f.do(http.MethodDelete, "/admin/api/projects/7/endpoints/"+strconv.Itoa(byKind["api"].ID), admin, ""); rec.Code != http.StatusOK {
		t.Errorf("deleting a data API: %d, want 200", rec.Code)
	}
}
