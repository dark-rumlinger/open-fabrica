package api

import (
	"strings"
	"testing"

	"openfabrica/tools"
)

func boolPtr(b bool) *bool { return &b }

func ownerColumnOf(t *testing.T, ctx *tools.ToolContext, path string) string {
	t.Helper()
	var col string
	if err := ctx.DB.QueryRow(
		"SELECT COALESCE(owner_column, '') FROM of_upload_endpoints WHERE path = ?", path,
	).Scan(&col); err != nil {
		t.Fatalf("reading owner_column for %q: %v", path, err)
	}
	return col
}

// The gap this closes: create SET owner_column but update could not, and create on an
// existing path refuses every option and points at update. So an endpoint built without
// scoping could never gain it - the tool's own remedy led to an action that lacked the arg.
func TestUpdateUpload_CanAddOwnerColumnAfterCreate(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustCreateUploadTable(t, db, "docs")
	if _, err := db.Exec(`ALTER TABLE docs ADD COLUMN user_id INTEGER`); err != nil {
		t.Fatalf("add user_id: %v", err)
	}

	if _, err := epCreateUpload(ctx, "docs", "docs", true, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := ownerColumnOf(t, ctx, "docs"); got != "" {
		t.Fatalf("expected no scoping after a create that omitted it, got %q", got)
	}

	res, err := epUpdateUpload(ctx, "docs", "", nil, 0, nil, 0, "user_id")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Success {
		t.Fatalf("adding owner_column after create must succeed, got %s", res.Error)
	}
	if got := ownerColumnOf(t, ctx, "docs"); got != "user_id" {
		t.Errorf("owner_column = %q, want user_id", got)
	}

	// And a sentinel clears it again, mirroring how table_name detaches.
	if _, err := epUpdateUpload(ctx, "docs", "", nil, 0, nil, 0, "none"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := ownerColumnOf(t, ctx, "docs"); got != "" {
		t.Errorf("owner_column should be cleared by the \"none\" sentinel, got %q", got)
	}
}

// The stamp reads the uploader from the request's auth token. Without requires_auth
// there is no user, so the column is written empty and a "my uploads" view silently
// shows everyone everything - the exact failure the setting exists to prevent.
func TestUpload_OwnerColumnRequiresAuth(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustCreateUploadTable(t, db, "docs")
	if _, err := db.Exec(`ALTER TABLE docs ADD COLUMN user_id INTEGER`); err != nil {
		t.Fatalf("add user_id: %v", err)
	}

	res, err := epCreateUpload(ctx, "open", "docs", false, 0, nil, 0, "user_id")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Success {
		t.Fatal("owner_column on an unauthenticated endpoint must be refused, not silently accepted")
	}
	if !strings.Contains(res.Error, "requires_auth") {
		t.Errorf("the rejection must name the missing precondition, got %q", res.Error)
	}
}

// owner_column is judged against the endpoint as it will be after the call, so
// turning auth ON in the same update is enough to make the scoping valid.
func TestUpdateUpload_OwnerColumnJudgedAgainstThePostUpdateEndpoint(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustCreateUploadTable(t, db, "docs")
	if _, err := db.Exec(`ALTER TABLE docs ADD COLUMN user_id INTEGER`); err != nil {
		t.Fatalf("add user_id: %v", err)
	}
	if _, err := epCreateUpload(ctx, "docs", "docs", false, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Auth is off right now; refused on the current state alone.
	res, _ := epUpdateUpload(ctx, "docs", "", nil, 0, nil, 0, "user_id")
	if res.Success {
		t.Fatal("owner_column must be refused while the endpoint is unauthenticated")
	}

	// Same call, turning auth on alongside it: now valid.
	res, err := epUpdateUpload(ctx, "docs", "", boolPtr(true), 0, nil, 0, "user_id")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !res.Success {
		t.Fatalf("enabling auth in the same call must satisfy the precondition, got %s", res.Error)
	}
}

// A column that does not exist on the linked table would stamp nowhere.
func TestUpdateUpload_RejectsUnknownOwnerColumn(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustCreateUploadTable(t, db, "docs")
	if _, err := epCreateUpload(ctx, "docs", "docs", true, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, _ := epUpdateUpload(ctx, "docs", "", nil, 0, nil, 0, "ghost_col")
	if res.Success {
		t.Fatal("an owner_column absent from the linked table must be refused")
	}
}

// list must report the scoping: without it nothing can answer "is this endpoint
// already my-uploads-only?" short of reading the table by hand.
func TestListUpload_ReportsOwnerColumn(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustCreateUploadTable(t, db, "docs")
	if _, err := db.Exec(`ALTER TABLE docs ADD COLUMN user_id INTEGER`); err != nil {
		t.Fatalf("add user_id: %v", err)
	}
	if _, err := epCreateUpload(ctx, "docs", "docs", true, 0, nil, 0, "user_id"); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := epListUpload(ctx)
	if err != nil || !res.Success {
		t.Fatalf("list: err=%v res=%+v", err, res)
	}
	rows, _ := res.Data.([]map[string]any)
	if len(rows) != 1 {
		t.Fatalf("expected one endpoint, got %d", len(rows))
	}
	if got, _ := rows[0]["owner_column"].(string); got != "user_id" {
		t.Errorf("list should report owner_column=user_id, got %q", got)
	}
}
