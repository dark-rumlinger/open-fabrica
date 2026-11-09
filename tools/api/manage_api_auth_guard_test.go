package api

import (
	"strings"
	"testing"

	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func runAPI(t *testing.T, a interface {
	Run(*tools.ToolContext) (*tools.Result, error)
}, ctx *tools.ToolContext) *tools.Result {
	t.Helper()
	res, err := a.Run(ctx)
	if err != nil || res == nil {
		t.Fatalf("run: err=%v res=%v", err, res)
	}
	return res
}

// required_role means something only with auth_model=role. Passing it with any other
// model used to create a PUBLIC endpoint while the call reported success.
func TestAPICreate_RequiredRoleWithoutRoleModelIsRefused(t *testing.T) {
	ctx := &tools.ToolContext{DB: setupAPIRoundTripDB(t)}
	res := runAPI(t, &APICreate{EndpointPath: "items", TableName: "items", RequiredRole: "admin"}, ctx)
	if res.Success || !strings.Contains(res.Error, "required_role only applies with auth_model=role") {
		t.Fatalf("expected a refusal naming the role model, got %+v", res)
	}
}

// Naming auth_model=owner on an endpoint that stores no owner_column used to flip it to
// plain authenticated, silently dropping the per-user scope. A stored column is kept
// when only the model is re-sent.
func TestAPIUpdate_OwnerKeepsStoredColumnAndRefusesWithoutOne(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, owner_column) VALUES ('mine','items','["GET"]',1,'user_id')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth) VALUES ('plain','items','["GET"]',1)`); err != nil {
		t.Fatal(err)
	}

	if res := runAPI(t, &APIUpdate{EndpointPath: "mine", AuthModel: "owner"}, ctx); !res.Success {
		t.Fatalf("re-sending owner must keep the stored column: %+v", res)
	}
	var owner string
	if err := db.QueryRow(`SELECT COALESCE(owner_column,'') FROM of_api_endpoints WHERE path='mine'`).Scan(&owner); err != nil || owner != "user_id" {
		t.Fatalf("owner_column = %q (%v), want user_id kept", owner, err)
	}

	res := runAPI(t, &APIUpdate{EndpointPath: "plain", AuthModel: "owner"}, ctx)
	if res.Success || !strings.Contains(res.Error, "owner_column") {
		t.Fatalf("owner with no column anywhere must be refused, got %+v", res)
	}
}

// Auth settings that ride an update without auth_model are ignored, so they are refused
// instead of reported as applied.
func TestAPIUpdate_AuthSettingsWithoutModelAreRefused(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, methods) VALUES ('open','items','["GET"]')`); err != nil {
		t.Fatal(err)
	}
	res := runAPI(t, &APIUpdate{EndpointPath: "open", ReadIsPublic: true}, ctx)
	if res.Success || !strings.Contains(res.Error, "auth_model rides the same call") {
		t.Fatalf("expected a refusal, got %+v", res)
	}
	if res := runAPI(t, &APIUpdate{EndpointPath: "open", RateLimit: 60}, ctx); !res.Success {
		t.Fatalf("a metadata-only update must still work: %+v", res)
	}
}

// A view cannot be scoped; asking for an auth model on one is refused rather than dropped.
func TestAPICreate_AuthModelOnViewIsRefused(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`CREATE VIEW v_rev AS SELECT 1 AS n`); err != nil {
		t.Fatal(err)
	}
	res := runAPI(t, &APICreate{EndpointPath: "rev", TableName: "v_rev", AuthModel: "authenticated"}, ctx)
	if res.Success || !strings.Contains(res.Error, "is a view") {
		t.Fatalf("expected a refusal, got %+v", res)
	}
}

// list shows the derived model and the flags an update must re-send.
func TestAPIList_ShowsAuthModelAndPublicWrite(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, public_write) VALUES ('contact','items','["POST"]',1,1)`); err != nil {
		t.Fatal(err)
	}
	res, err := epListAPI(ctx)
	if err != nil || !res.Success {
		t.Fatalf("list: %v %+v", err, res)
	}
	got := tooltest.DataJSON(res)
	for _, want := range []string{`"auth_model":"authenticated"`, `"public_write":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("list is missing %s:\n%s", want, got)
		}
	}
}

// enable_query records its endpoint outside of_api_endpoints, so delete used to report
// not-found and the free-form SELECT endpoint could never be switched off. A public
// one also says what it exposes.
func TestAPIEnableQuery_WarnsWhenPublicAndDeleteRemovesIt(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	res := runAPI(t, &APIEnableQuery{EndpointPath: "_query"}, ctx)
	if !res.Success {
		t.Fatalf("enable_query: %+v", res)
	}
	data, _ := res.Data.(map[string]any)
	if w, _ := data["warning"].(string); !strings.Contains(w, "requires_auth=true") {
		t.Fatalf("a public query endpoint must warn about what it exposes, got %v", data)
	}

	if del := runAPI(t, &APIDelete{EndpointPath: "_query"}, ctx); !del.Success {
		t.Fatalf("delete must remove the query endpoint: %+v", del)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM of_query_endpoints WHERE path='_query'`).Scan(&n)
	if n != 0 {
		t.Fatal("the query endpoint row survived delete")
	}

	authed := runAPI(t, &APIEnableQuery{EndpointPath: "q2", RequiresAuth: true}, ctx)
	if d, _ := authed.Data.(map[string]any); d["warning"] != nil {
		t.Fatalf("an authenticated query endpoint needs no exposure warning: %v", d)
	}
}
