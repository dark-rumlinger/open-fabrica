package api

import (
	"testing"

	"openfabrica/tools"
)

// TestAPICreateAutoScopesOwnerLikeTables reproduces an rls_scope leak class: the
// agent creates writable, auth-gated CRUD endpoints over row-owned tables
// (e.g. channels, messages, reactions) whose owner reference
// (user_id / created_by / author_id) is a plain column with NO formal foreign
// key, so the FK auto-detect finds nothing and the endpoints are left unscoped -
// every signed-in user can read AND write every other user's rows, caught only
// by the post-build gate.
//
// The prevent-at-source guard in epCreateAPI (ownerLikeColumnForTable, the same
// store.OwnerLikeColumn heuristic the build-end validator consumes) must adopt
// that owner-like column as owner_column at create time so no fix-pass is needed.
func TestAPICreateAutoScopesOwnerLikeTables(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	// Auth exists (the app has login), so requiresAuth is meaningful - but the
	// tables carry NO FK to the auth table, exactly how the real leak slipped the
	// FK-based auto-detect and fell to the owner-like heuristic.
	if _, err := db.Exec(`INSERT INTO of_auth_endpoints (path, table_name, username_column) VALUES ('auth','users','email')`); err != nil {
		t.Fatalf("seed auth endpoint: %v", err)
	}

	// Physical tables with an owner-like column and no foreign key. The expected
	// owner_column is the FIRST owner-like column in PRAGMA definition order.
	tables := []struct {
		table     string
		ddl       string
		wantOwner string
	}{
		{"channel_members", `CREATE TABLE channel_members (user_id INTEGER NOT NULL, channel_id INTEGER NOT NULL, joined_at TEXT)`, "user_id"},
		{"channels", `CREATE TABLE channels (id INTEGER PRIMARY KEY, name TEXT, created_by INTEGER)`, "created_by"},
		{"messages", `CREATE TABLE messages (id INTEGER PRIMARY KEY, channel_id INTEGER, author_id INTEGER, body TEXT)`, "author_id"},
		{"reactions", `CREATE TABLE reactions (message_id INTEGER, user_id INTEGER, emoji TEXT)`, "user_id"},
	}
	for _, tc := range tables {
		if _, err := db.Exec(tc.ddl); err != nil {
			t.Fatalf("create %s: %v", tc.table, err)
		}
		// Register as a dynamic table so create_api accepts it as a backing table.
		if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES (?, '[]')`, tc.table); err != nil {
			t.Fatalf("register %s: %v", tc.table, err)
		}
	}

	ctx := &tools.ToolContext{DB: db}
	for _, tc := range tables {
		t.Run(tc.table, func(t *testing.T) {
			// Authenticated (requiresAuth true), non-public-read, no owner_column set -
			// the call shape that produces the leak.
			res, err := (&APICreate{EndpointPath: tc.table, TableName: tc.table, AuthModel: "authenticated"}).Run(ctx)
			if err != nil || res == nil || !res.Success {
				t.Fatalf("create %s: err=%v res=%+v", tc.table, err, res)
			}

			var ownerCol string
			if err := db.QueryRow(`SELECT COALESCE(owner_column,'') FROM of_api_endpoints WHERE path = ?`, tc.table).Scan(&ownerCol); err != nil {
				t.Fatalf("load %s row: %v", tc.table, err)
			}
			if ownerCol != tc.wantOwner {
				t.Errorf("%s: endpoint shipped unscoped (owner_column=%q, want %q) - the rls_scope leak", tc.table, ownerCol, tc.wantOwner)
			}

			data, ok := res.Data.(map[string]any)
			if !ok {
				t.Fatalf("%s: result data is not a map: %T", tc.table, res.Data)
			}
			if applied, _ := data["owner_scope_applied"].(string); applied == "" {
				t.Errorf("%s: expected owner_scope_applied to tell the agent the endpoint was auto-scoped, got none", tc.table)
			}
		})
	}
}

// TestAPICreateDoesNotOverScopePlainTables guards the other side: a table with no
// owner-like column (a shared catalog) must NOT be silently narrowed, so the
// auto-scope guard never turns world-visible reference data into per-user data.
func TestAPICreateDoesNotOverScopePlainTables(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	if _, err := db.Exec(`CREATE TABLE emoji_catalog (id INTEGER PRIMARY KEY, name TEXT, unicode TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('emoji_catalog', '[]')`); err != nil {
		t.Fatalf("register table: %v", err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{EndpointPath: "emoji_catalog", TableName: "emoji_catalog", AuthModel: "authenticated"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}

	var ownerCol string
	if err := db.QueryRow(`SELECT COALESCE(owner_column,'') FROM of_api_endpoints WHERE path = ?`, "emoji_catalog").Scan(&ownerCol); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if ownerCol != "" {
		t.Errorf("plain catalog table was over-scoped to owner_column=%q; auto-scope must only fire on owner-like columns", ownerCol)
	}
	if data, ok := res.Data.(map[string]any); ok {
		if applied, _ := data["owner_scope_applied"].(string); applied != "" {
			t.Errorf("owner_scope_applied should be absent for a non-owner-like table, got %q", applied)
		}
	}
}

// TestAPICreatePublicReadKeepsFKTableUnscoped guards the FK auto-detect from
// narrowing a deliberately public endpoint. A public blog whose posts carry an
// author_id FK to the user table is the shape: adopting owner_column there also
// forces requires_auth, and the runtime applies the owner filter whenever an
// optional token happens to be present - so anonymous visitors see every post
// while the signed-in author sees only their own.
func TestAPICreatePublicReadKeepsFKTableUnscoped(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	if _, err := db.Exec(`INSERT INTO of_auth_endpoints (path, table_name, username_column) VALUES ('auth','users','email')`); err != nil {
		t.Fatalf("seed auth endpoint: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE posts (id INTEGER PRIMARY KEY, title TEXT, author_id INTEGER REFERENCES users(id))`); err != nil {
		t.Fatalf("create posts: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('posts','[]')`); err != nil {
		t.Fatalf("register posts: %v", err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{EndpointPath: "posts", TableName: "posts", AuthModel: "public", ReadIsPublic: true}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}

	var ownerCol string
	var requiresAuth int
	if err := db.QueryRow(`SELECT COALESCE(owner_column,''), COALESCE(requires_auth,0) FROM of_api_endpoints WHERE path = 'posts'`).Scan(&ownerCol, &requiresAuth); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if ownerCol != "" {
		t.Errorf("a public_read endpoint was silently narrowed to owner_column=%q", ownerCol)
	}
	if requiresAuth != 0 {
		t.Errorf("a public endpoint was silently force-authed (requires_auth=%d)", requiresAuth)
	}
}

// TestAPICreateFKAutoScopesAuthedEndpoint is the counterweight: tightening the
// FK guard must not turn the auto-scope into a no-op where it belongs.
func TestAPICreateFKAutoScopesAuthedEndpoint(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	if _, err := db.Exec(`INSERT INTO of_auth_endpoints (path, table_name, username_column) VALUES ('auth','users','email')`); err != nil {
		t.Fatalf("seed auth endpoint: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)`); err != nil {
		t.Fatalf("create users: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT, owner INTEGER REFERENCES users(id))`); err != nil {
		t.Fatalf("create notes: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('notes','[]')`); err != nil {
		t.Fatalf("register notes: %v", err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{EndpointPath: "notes", TableName: "notes", AuthModel: "authenticated"}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}
	var ownerCol string
	if err := db.QueryRow(`SELECT COALESCE(owner_column,'') FROM of_api_endpoints WHERE path = 'notes'`).Scan(&ownerCol); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if ownerCol != "owner" {
		t.Errorf("FK auto-scope did not fire on an authed endpoint: owner_column=%q, want %q", ownerCol, "owner")
	}
}
