package api

import (
	"testing"

	"openfabrica/tools"
)

// TestAPICreateOverView verifies a SQL VIEW backs a read-only GET endpoint:
// methods are forced to GET and read_only is persisted, so the runtime serves
// the view's rows without exposing writes.
func TestAPICreateOverView(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	if _, err := db.Exec(`CREATE VIEW item_summary AS SELECT table_name FROM of_dynamic_tables`); err != nil {
		t.Fatalf("create view: %v", err)
	}
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APICreate{
		EndpointPath: "summary",
		TableName:    "item_summary",
		// Caller asks for full CRUD; the view fallback must override to GET only.
		Methods: []string{"GET", "POST", "PUT", "DELETE"},
	}).Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create over view: err=%v res=%+v", err, res)
	}
	if res.Data.(map[string]any)["read_only"] != true {
		t.Errorf("expected read_only=true in result, got %+v", res.Data)
	}

	var methods string
	var readOnly bool
	if err := db.QueryRow(`SELECT methods, read_only FROM of_api_endpoints WHERE path = ?`, "summary").Scan(&methods, &readOnly); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if methods != `["GET"]` {
		t.Errorf("view endpoint should be GET-only, got methods=%q", methods)
	}
	if !readOnly {
		t.Errorf("view endpoint should be read_only")
	}
}

// TestAPICreateUnknownObject rejects a target that is neither a table nor a view.
func TestAPICreateUnknownObject(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := (&APICreate{EndpointPath: "nope", TableName: "does_not_exist"}).Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("expected failure for unknown object, got %+v", res)
	}
}

// TestEnableQuery persists the opt-in row with a clamped row cap.
func TestEnableQuery(t *testing.T) {
	db := setupAPIRoundTripDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&APIEnableQuery{MaxRows: 99999}).Run(ctx) // over the cap -> clamped
	if err != nil || res == nil || !res.Success {
		t.Fatalf("enable_query: err=%v res=%+v", err, res)
	}

	var path string
	var maxRows, rateLimit int
	if err := db.QueryRow(`SELECT path, max_rows, rate_limit FROM of_query_endpoints`).Scan(&path, &maxRows, &rateLimit); err != nil {
		t.Fatalf("load row: %v", err)
	}
	if path != "_query" {
		t.Errorf("default path should be _query, got %q", path)
	}
	if maxRows != 1000 {
		t.Errorf("over-cap max_rows should clamp to the documented max 1000, got %d", maxRows)
	}
	if rateLimit != 60 {
		t.Errorf("default rate_limit should be 60, got %d", rateLimit)
	}

	// enable_query is idempotent: a second call updates in place, not a 2nd row.
	if _, err := (&APIEnableQuery{MaxRows: 50}).Run(ctx); err != nil {
		t.Fatalf("second enable_query: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM of_query_endpoints`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("enable_query should upsert, got %d rows", count)
	}
}
