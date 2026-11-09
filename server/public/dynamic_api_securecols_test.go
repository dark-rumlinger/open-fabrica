package public

import "testing"

// loadEndpoint must carry the table's PASSWORD/ENCRYPTED projection, and must
// refuse the endpoint when that projection cannot be read: serving rows with
// secret columns left unstripped is worse than failing the request.
func TestLoadEndpointSecureColumnsFailClosed(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods) VALUES ('members','members','["GET"]')`)
	mustExec(t, w, `INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES ('members','{}','{"password":"hash"}')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}

	ep, err := h.loadEndpoint(pdb.Reader(), "members")
	if err != nil {
		t.Fatalf("loadEndpoint: %v", err)
	}
	if ep.SecureCols["password"] != "hash" {
		t.Fatalf("SecureCols = %v, want password=hash", ep.SecureCols)
	}

	// Make the secure-column lookup itself fail.
	mustExec(t, w, `DROP TABLE of_dynamic_tables`)
	if ep, err := h.loadEndpoint(pdb.Reader(), "members"); err == nil {
		t.Fatalf("loadEndpoint served the endpoint with SecureCols=%v after the lookup failed; want an error", ep.SecureCols)
	}
}
