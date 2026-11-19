package public

import (
	"errors"
	"testing"
)

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

// An owner or tenant column that cannot be used must stop the endpoint, not
// drop the row filter.
func TestLoadEndpointInvalidScopeColumnFailsClosed(t *testing.T) {
	mgr, pdb := spineDispatchDB(t)
	w := pdb.Writer()
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, owner_column) VALUES ('notes','notes','["GET"]','user id; --')`)
	mustExec(t, w, `INSERT INTO of_api_endpoints (path, table_name, methods, tenant_column) VALUES ('orgs','orgs','["GET"]','1bad')`)
	h := &Handler{deps: &Deps{ProjectDBManager: mgr}}
	for _, path := range []string{"notes", "orgs"} {
		if _, err := h.loadEndpoint(pdb.Reader(), path); !errors.Is(err, errEndpointMisconfigured) {
			t.Errorf("%s: err = %v, want errEndpointMisconfigured", path, err)
		}
	}
}
