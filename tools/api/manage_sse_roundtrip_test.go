package api

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupSSEDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func setupSSEDB(t *testing.T) dbh.Handle {
	db := tooltest.ProjectDB(t)
	// table_name scopes are checked against the project's tables.
	for _, name := range []string{"posts", "comments", "orders"} {
		if _, err := db.Exec(`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES (?, '{}')`, name); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	return db
}

// A table_name that is not a project table is refused instead of streaming nothing.
func TestSSECreateRejectsUnknownTable(t *testing.T) {
	ctx := &tools.ToolContext{DB: setupSSEDB(t)}
	r, _ := (&SSECreate{EndpointPath: "feed", TableName: "orders/recent"}).Run(ctx)
	if r == nil || r.Success {
		t.Fatalf("an unknown table_name must be refused, got %+v", r)
	}
}

// TestSSEUpdateTriState pins the tri-state update semantics: requires_auth
// and table_name patch only when passed, `requires_auth: false` disables an
// enabled gate, and `table_name: ""` widens a table-scoped stream back to
// every data event (a plain bool/string merge could express neither).
func TestSSEUpdateTriState(t *testing.T) {
	db := setupSSEDB(t)
	ctx := &tools.ToolContext{DB: db}
	boolPtr := func(b bool) *bool { return &b }
	strPtr := func(s string) *string { return &s }

	if r, err := (&SSECreate{EndpointPath: "feed", RequiresAuth: true, TableName: "posts"}).Run(ctx); err != nil || r == nil || !r.Success {
		t.Fatalf("create: err=%v res=%+v", err, r)
	}

	load := func() (auth bool, table string) {
		t.Helper()
		if err := db.QueryRow("SELECT requires_auth, COALESCE(table_name,'') FROM of_stream_endpoints WHERE path = ?", "feed").Scan(&auth, &table); err != nil {
			t.Fatalf("load row: %v", err)
		}
		return auth, table
	}

	// Omitting both pointers must leave them untouched.
	if r, err := (&SSEUpdate{EndpointPath: "feed", EventTypes: []any{"data.insert"}}).Run(ctx); err != nil || r == nil || !r.Success {
		t.Fatalf("update event_types: err=%v res=%+v", err, r)
	}
	if auth, table := load(); !auth || table != "posts" {
		t.Fatalf("omitted fields changed: auth=%v table=%q", auth, table)
	}

	// requires_auth: false must disable the gate.
	if r, err := (&SSEUpdate{EndpointPath: "feed", RequiresAuth: boolPtr(false)}).Run(ctx); err != nil || r == nil || !r.Success {
		t.Fatalf("disable auth: err=%v res=%+v", err, r)
	}
	if auth, _ := load(); auth {
		t.Error("requires_auth=false did not disable the auth gate")
	}

	// table_name: "" must widen the scope back to all tables.
	if r, err := (&SSEUpdate{EndpointPath: "feed", TableName: strPtr("")}).Run(ctx); err != nil || r == nil || !r.Success {
		t.Fatalf("widen scope: err=%v res=%+v", err, r)
	}
	if _, table := load(); table != "" {
		t.Errorf("table_name=\"\" did not widen the scope: %q", table)
	}

	// And back on/scoped again, to prove both directions.
	if r, err := (&SSEUpdate{EndpointPath: "feed", RequiresAuth: boolPtr(true), TableName: strPtr("comments")}).Run(ctx); err != nil || r == nil || !r.Success {
		t.Fatalf("re-enable: err=%v res=%+v", err, r)
	}
	if auth, table := load(); !auth || table != "comments" {
		t.Errorf("re-enable failed: auth=%v table=%q", auth, table)
	}
}

// The plan names an SSE task's deliverable by its route (`orders/stream`), and the
// model passed that on - rejected as reserved_path in some builds. The suffix
// can only mean the stream route the runtime adds, so it is read as the resource.
func TestSSECreateAcceptsRouteForm(t *testing.T) {
	db := setupSSEDB(t)
	ctx := &tools.ToolContext{DB: db}
	r, err := (&SSECreate{EndpointPath: "orders/stream", TableName: "orders"}).Run(ctx)
	if err != nil || r == nil || !r.Success {
		t.Fatalf("create with route form: err=%v res=%+v", err, r)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM of_stream_endpoints WHERE path = 'orders'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("stream must be stored under the bare resource, count=%d err=%v", n, err)
	}
	if r, err := (&SSEUpdate{EndpointPath: "orders/stream", EventTypes: []any{"data.insert"}}).Run(ctx); err != nil || r == nil || !r.Success {
		t.Fatalf("update with route form: err=%v res=%+v", err, r)
	}
}
