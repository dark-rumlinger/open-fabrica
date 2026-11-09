package auth

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupTenancyDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)

	stmts := []string{
		// Domain tables used by add_tenant_column tests.
		`CREATE TABLE notes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT
		)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('notes', '{"title":"text"}')`,
		`INSERT INTO of_api_endpoints (path, table_name, owner_column, tenant_column) VALUES ('notes', 'notes', 'user_id', NULL)`,
		`INSERT INTO of_auth_endpoints (path, table_name) VALUES ('auth', 'users')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	return db
}

func TestTenancyEnable_SetsSignupMode(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := tenancyEnable(ctx, "auth", "invite_only", "")
	if !res.Success {
		t.Fatalf("enable failed: %s", res.Error)
	}
	var mode string
	if err := db.QueryRow("SELECT tenant_signup_mode FROM of_auth_endpoints WHERE path = 'auth'").Scan(&mode); err != nil {
		t.Fatalf("read mode: %v", err)
	}
	if mode != "invite_only" {
		t.Errorf("expected invite_only, got %q", mode)
	}
}

func TestTenancyEnable_RejectsUnknownAuthEndpoint(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := tenancyEnable(ctx, "nope", "invite_only", "")
	if res.Success {
		t.Fatal("expected failure on missing auth endpoint")
	}
}

func TestTenancyAddTenantColumn_AddsAndPropagates(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := tenancyAddTenantColumn(ctx, []any{"notes"}, "")
	if !res.Success {
		t.Fatalf("add_tenant_column failed: %s", res.Error)
	}

	// Column should exist on the domain table.
	rows, err := db.Query("PRAGMA table_info(notes)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, ct string
		var nn int
		var dflt any
		var pk int
		_ = rows.Scan(&cid, &name, &ct, &nn, &dflt, &pk)
		if name == "tenant_id" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if !found {
		t.Error("expected tenant_id column on notes")
	}

	// Endpoint config should now name the tenant column.
	var col string
	if err := db.QueryRow("SELECT COALESCE(tenant_column,'') FROM of_api_endpoints WHERE path = 'notes'").Scan(&col); err != nil {
		t.Fatalf("read tenant_column: %v", err)
	}
	if col != "tenant_id" {
		t.Errorf("expected tenant_column=tenant_id, got %q", col)
	}
}

func TestTenancyAddTenantColumn_Idempotent(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	for i := 0; i < 3; i++ {
		res, _ := tenancyAddTenantColumn(ctx, []any{"notes"}, "")
		if !res.Success {
			t.Fatalf("iteration %d: %s", i, res.Error)
		}
	}
}

func TestTenancyAddTenantColumn_RefusesSystemTables(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := tenancyAddTenantColumn(ctx, []any{"of_files"}, "")
	if res.Success {
		t.Fatal("expected refusal on system table")
	}
	if !strings.Contains(strings.ToLower(res.Error), "system table") {
		t.Errorf("unexpected error: %q", res.Error)
	}
}

func TestTenancyCreateTenant_AutoSlugAndIdempotent(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	r1, _ := tenancyCreateTenant(ctx, "", "Acme Inc", "", nil)
	if !r1.Success {
		t.Fatalf("create: %s", r1.Error)
	}
	d1 := r1.Data.(map[string]any)
	if d1["slug"] != "acme-inc" {
		t.Errorf("slug autogen wrong: %v", d1["slug"])
	}
	// Second call with the same slug must return the existing id rather than failing.
	r2, _ := tenancyCreateTenant(ctx, "acme-inc", "Acme Inc", "", nil)
	if !r2.Success {
		t.Fatalf("rerun: %s", r2.Error)
	}
}

func TestTenancyInviteMember_StoresAndReturnsToken(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	rt, _ := tenancyCreateTenant(ctx, "", "Acme", "", nil)
	tenantID := int(rt.Data.(map[string]any)["tenant_id"].(int64))

	r, _ := tenancyInviteMember(ctx, tenantID, "alice@acme.com", "admin")
	if !r.Success {
		t.Fatalf("invite: %s", r.Error)
	}
	token := r.Data.(map[string]any)["invite_token"].(string)
	if len(token) < 32 {
		t.Errorf("token looks weak: %q", token)
	}

	var storedRole string
	if err := db.QueryRow("SELECT role FROM of_tenant_invites WHERE token = ?", token).Scan(&storedRole); err != nil {
		t.Fatalf("lookup invite: %v", err)
	}
	if storedRole != "admin" {
		t.Errorf("role not persisted: %q", storedRole)
	}
}

// add_tenant_column stamps only endpoints that exist when it runs. The result says how
// many endpoints each table has scoped, and a table with none gets a warning instead of
// reading as success.
func TestTenancyAddTenantColumn_ReportsScopedEndpointsAndGaps(t *testing.T) {
	db := setupTenancyDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`CREATE TABLE orphan (id INTEGER PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('orphan', '{"title":"text"}')`); err != nil {
		t.Fatal(err)
	}
	res, _ := tenancyAddTenantColumn(ctx, []any{"notes", "orphan"}, "")
	if !res.Success {
		t.Fatalf("add_tenant_column: %s", res.Error)
	}
	data := res.Data.(map[string]any)
	scoped := data["endpoints_scoped"].(map[string]int)
	if scoped["notes"] != 1 || scoped["orphan"] != 0 {
		t.Fatalf("endpoints_scoped = %v, want notes:1 orphan:0", scoped)
	}
	if w, _ := data["warning"].(string); !strings.Contains(w, "orphan") || strings.Contains(w, "notes,") {
		t.Fatalf("warning must name the unscoped table only, got %q", w)
	}
}
