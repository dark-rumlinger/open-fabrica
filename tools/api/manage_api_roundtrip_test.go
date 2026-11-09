package api

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/store"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupAPIRoundTripDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('items','[]')`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	return db
}

// TestAPICreateRoundTrip pins every declared APICreate arg through to the stored
// of_api_endpoints row, so no arg is silently dropped on the typed handler.
func TestAPICreateRoundTrip(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	act := &APICreate{
		EndpointPath:  "items",
		TableName:     "items",
		Methods:       []string{"GET", "POST"},
		PublicColumns: []string{"id", "name"},
		AuthModel:     "role",
		RequiredRole:  "editor",
		ReadIsPublic:  true,
		RateLimit:     120,
		CacheTTL:      30,
		LookupColumn:  "slug",
		CORSOrigins:   []string{"https://a.example", "https://b.example"},
		CORSMethods:   []string{"GET", "POST"},
		CORSHeaders:   []string{"X-Token"},
	}
	res, err := act.Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}

	var methods, pubCols, requiredRole, corsOrigins, corsMethods, corsHeaders, lookupCol string
	var requiresAuth, publicRead bool
	var rateLimit, cacheTTL int
	if err := db.QueryRow(`SELECT methods, public_columns, requires_auth, public_read,
		COALESCE(required_role,''), rate_limit, cache_ttl, cors_origins, cors_methods, cors_headers,
		COALESCE(lookup_column,'') FROM of_api_endpoints WHERE path = ?`, "items").Scan(
		&methods, &pubCols, &requiresAuth, &publicRead, &requiredRole, &rateLimit, &cacheTTL,
		&corsOrigins, &corsMethods, &corsHeaders, &lookupCol); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if methods != `["GET","POST"]` {
		t.Errorf("methods not stored: %q", methods)
	}
	if pubCols != `["id","name"]` {
		t.Errorf("public_columns not stored: %q", pubCols)
	}
	if !requiresAuth {
		t.Errorf("auth_model=role should set requires_auth")
	}
	if requiredRole != "editor" {
		t.Errorf("required_role not stored: %q", requiredRole)
	}
	if !publicRead {
		t.Errorf("public_read not stored")
	}
	if rateLimit != 120 {
		t.Errorf("rate_limit not stored: %d", rateLimit)
	}
	if cacheTTL != 30 {
		t.Errorf("cache_ttl not stored: %d", cacheTTL)
	}
	if lookupCol != "slug" {
		t.Errorf("lookup_column not stored: %q", lookupCol)
	}
	if corsOrigins != "https://a.example,https://b.example" {
		t.Errorf("cors_origins not stored: %q", corsOrigins)
	}
	if corsMethods != "GET,POST" {
		t.Errorf("cors_methods not stored: %q", corsMethods)
	}
	if corsHeaders != "X-Token" {
		t.Errorf("cors_headers not stored: %q", corsHeaders)
	}
}

// TestAPIPublicWriteRoundTrip covers the public-submission shape (contact form:
// anyone POSTs, only an admin reads). It pins three things: (1) create stores
// public_write with requires_auth so writes are open while GET stays gated;
// (2) update rewrites the FULL auth shape - it must be able to relax auth, not
// just set flags to true (a set-only update would silently no-op and force a
// delete+recreate);
// (3) the contradictory public_write combinations are rejected at create time.
func TestAPIPublicWriteRoundTrip(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	// (1) create: contact-form endpoint = auth-gated reads, open writes.
	res, err := (&APICreate{
		EndpointPath:  "messages",
		TableName:     "items",
		Methods:       []string{"GET", "POST"},
		AuthModel:     "authenticated",
		WriteIsPublic: true,
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create public_write: err=%v res=%+v", err, res)
	}
	requiresAuth, publicRead, publicWrite := loadAuthFlags(t, db, "messages")
	if !requiresAuth || publicRead || !publicWrite {
		t.Fatalf("create should store requires_auth=1, public_read=0, public_write=1; got %v/%v/%v", requiresAuth, publicRead, publicWrite)
	}

	// (2) update back toward public: the whole auth shape must be rewritten,
	// including flags flipped back to false. This is the delete+recreate fix.
	if res, err := (&APIUpdate{EndpointPath: "messages", AuthModel: "public"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("update to public: err=%v res=%+v", err, res)
	}
	requiresAuth, _, publicWrite = loadAuthFlags(t, db, "messages")
	if requiresAuth {
		t.Errorf("update authenticated->public must clear requires_auth (the stale-flag bug), got requires_auth=1")
	}
	if publicWrite {
		t.Errorf("update to public must clear public_write, got public_write=1")
	}

	// (3a) public_write with auth_model=public is redundant and rejected.
	if res, _ := (&APICreate{EndpointPath: "x", TableName: "items", AuthModel: "public", WriteIsPublic: true}).Run(ctx); res == nil || res.Success {
		t.Errorf("public_write + auth_model=public should be rejected")
	}
	// (3b) public_write with owner scoping is contradictory and rejected.
	if res, _ := (&APICreate{EndpointPath: "y", TableName: "items", AuthModel: "owner", OwnerColumn: "user_id", WriteIsPublic: true}).Run(ctx); res == nil || res.Success {
		t.Errorf("public_write + auth_model=owner should be rejected")
	}
}

// loadAuthFlags reads the three per-endpoint auth booleans for an assertion.
func loadAuthFlags(t *testing.T, db dbh.Handle, path string) (requiresAuth, publicRead, publicWrite bool) {
	t.Helper()
	if err := db.QueryRow(`SELECT requires_auth, public_read, COALESCE(public_write,0) FROM of_api_endpoints WHERE path = ?`, path).
		Scan(&requiresAuth, &publicRead, &publicWrite); err != nil {
		t.Fatalf("load auth flags for %q: %v", path, err)
	}
	return
}

// TestAPICreateResponseContract pins that create returns the live response
// contract - the LIST envelope vs the bare-row shape, the single-row sub-route,
// and the real columns read back from the table - so the agent never has to
// learn it by failing a probe (asserting bare fields against the LIST envelope).
func TestAPICreateResponseContract(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	// A physical table so the contract can read back real columns.
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, slug TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("create physical table: %v", err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{EndpointPath: "items", TableName: "items", Methods: []string{"GET", "POST"}, LookupColumn: "slug"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("result data is not a map: %T", res.Data)
	}
	contract, ok := data["response_contract"].(map[string]any)
	if !ok {
		t.Fatalf("expected response_contract in create result, got %+v", data)
	}
	list, ok := contract["list"].(map[string]any)
	if !ok || list["route"] != "GET /api/items" {
		t.Fatalf("list contract wrong: %+v", contract["list"])
	}
	row, ok := contract["row"].(map[string]any)
	if !ok || row["route"] != "GET /api/items/<slug>" {
		t.Fatalf("row contract should use the lookup column in the sub-route: %+v", contract["row"])
	}
	cols, ok := row["columns"].([]store.ColumnInfo)
	if !ok {
		t.Fatalf("row contract should carry read-back columns, got %T", row["columns"])
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c.Name] = true
	}
	if !have["name"] || !have["slug"] {
		t.Errorf("expected real columns name+slug in the contract, got %+v", cols)
	}
}

// TestAPIUpdateTableNameRoundTrip pins the arg-forwarding drift fix: APIUpdate
// declares table_name but the prior handler never read it, so re-pointing an
// endpoint at a new table silently no-oped. The typed handler now patches it.
func TestAPIUpdateTableNameRoundTrip(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&APICreate{EndpointPath: "items", TableName: "items"}).Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}
	// An update to a table that does not exist is refused by name.
	if res, _ := (&APIUpdate{EndpointPath: "items", TableName: "nope"}).Run(ctx); res == nil || res.Success {
		t.Fatalf("update to a missing table must fail, got %+v", res)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('archive', '{}')`); err != nil {
		t.Fatalf("register archive table: %v", err)
	}
	res, err := (&APIUpdate{EndpointPath: "items", TableName: "archive", RateLimit: 99}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("update: err=%v res=%+v", err, res)
	}

	var tableName string
	var rateLimit int
	if err := db.QueryRow("SELECT table_name, rate_limit FROM of_api_endpoints WHERE path = ?", "items").Scan(&tableName, &rateLimit); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if tableName != "archive" {
		t.Fatalf("table_name not updated (arg-forwarding drift): %q", tableName)
	}
	if rateLimit != 99 {
		t.Errorf("rate_limit not updated: %d", rateLimit)
	}
}
