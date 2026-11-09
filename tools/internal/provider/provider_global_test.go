package provider

import (
	"database/sql"
	"net/http"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// connectorDBs is one migrated instance: the project database (of_service_providers,
// of_secrets, of_enabled_connectors) and the global one (global_connectors), each
// exactly as production has it.
func connectorDBs(t *testing.T) (proj dbh.Handle, global *sql.DB) {
	t.Helper()
	h := tooltest.New(t)
	return h.DB(), h.Global.DB
}

func exec(t *testing.T, db dbh.Handle, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestLoadGlobalConnectorFallback: a global connector is invisible until enabled,
// then resolves with its inline credential and InjectAuth signs the request.
func TestLoadGlobalConnectorFallback(t *testing.T) {
	enc := newTestEncryptor(t)
	proj, global := connectorDBs(t)
	encTok, err := enc.Encrypt("gtok")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	exec(t, global, `INSERT INTO global_connectors (name, base_url, auth_type, auth_header, auth_prefix, api_key_encrypted) VALUES ('shared','https://api.shared.com','bearer','Authorization','Bearer',?)`, encTok)

	ctx := &tools.ToolContext{DB: proj, GlobalDB: global, Encryptor: enc}

	if _, r := Load(ctx, "shared"); r == nil {
		t.Fatalf("expected not-found before the project enables the global connector")
	}

	exec(t, proj, "INSERT INTO of_enabled_connectors (global_name) VALUES ('shared')")

	c, r := Load(ctx, "shared")
	if r != nil {
		t.Fatalf("load global connector: %+v", r)
	}
	if c.BaseURL != "https://api.shared.com" || !c.InlineSecret.Valid {
		t.Fatalf("unexpected creds: %+v", c)
	}
	req, _ := http.NewRequest("GET", "https://api.shared.com", nil)
	if rr := InjectAuth(ctx, req, c, ""); rr != nil {
		t.Fatalf("InjectAuth: %+v", rr)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer gtok" {
		t.Fatalf("auth header from inline global credential = %q", got)
	}
}

// TestProjectConnectorShadowsGlobal: a project-local connector wins over a
// globally-enabled one of the same name.
func TestProjectConnectorShadowsGlobal(t *testing.T) {
	enc := newTestEncryptor(t)
	proj, global := connectorDBs(t)
	exec(t, proj, `INSERT INTO of_service_providers (name, base_url, auth_type) VALUES ('dup','https://project.example','none')`)
	exec(t, proj, `INSERT INTO of_enabled_connectors (global_name) VALUES ('dup')`)
	exec(t, global, `INSERT INTO global_connectors (name, base_url, auth_type) VALUES ('dup','https://global.example','none')`)

	ctx := &tools.ToolContext{DB: proj, GlobalDB: global, Encryptor: enc}
	c, r := Load(ctx, "dup")
	if r != nil {
		t.Fatalf("load: %+v", r)
	}
	if c.BaseURL != "https://project.example" {
		t.Fatalf("project-local should shadow global; resolved base_url = %q", c.BaseURL)
	}
}
