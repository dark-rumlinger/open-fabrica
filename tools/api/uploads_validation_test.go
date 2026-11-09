package api

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// mustCreateUploadTable creates a table with the four columns the upload
// handler auto-inserts, so an upload endpoint can legitimately bind to it.
func mustCreateUploadTable(t *testing.T, db dbh.Handle, name string) {
	t.Helper()
	_, err := db.Exec("CREATE TABLE " + name + ` (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		filename TEXT NOT NULL,
		url TEXT,
		content_type TEXT,
		size INTEGER
	)`)
	if err != nil {
		t.Fatalf("create table %s: %v", name, err)
	}
}

func endpointTableName(t *testing.T, db dbh.Handle, path string) string {
	t.Helper()
	var tn string
	if err := db.QueryRow("SELECT COALESCE(table_name,'') FROM of_upload_endpoints WHERE path = ?", path).Scan(&tn); err != nil {
		t.Fatalf("load endpoint %s: %v", path, err)
	}
	return tn
}

// TestCreateUpload_RejectsNonexistentTable pins the fatal-bug fix: binding an
// upload endpoint to a table that doesn't exist would 500 every upload at
// request time ("no such table"). Reject it up front instead.
func TestCreateUpload_RejectsNonexistentTable(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := epCreateUpload(ctx, "files", "ghost", false, 0, nil, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection binding to a non-existent table")
	}
	if !strings.Contains(res.Error, "does not exist") {
		t.Errorf("expected 'does not exist' in error, got %q", res.Error)
	}
}

// TestCreateUpload_NoneSentinelIsDiskOnly: passing "none" (or "null"/"-") for
// table_name means disk-only storage - it is normalized to an empty binding,
// never treated as a literal table called "none".
func TestCreateUpload_NoneSentinelIsDiskOnly(t *testing.T) {
	for _, sentinel := range []string{"none", "NONE", "null", "-", " none "} {
		db := setupUploadDB(t)
		ctx := &tools.ToolContext{DB: db}
		res, err := epCreateUpload(ctx, "drop", sentinel, false, 0, nil, 0, "")
		if err != nil || !res.Success {
			t.Fatalf("sentinel %q: expected success, got err=%v res=%+v", sentinel, err, res)
		}
		if tn := endpointTableName(t, db, "drop"); tn != "" {
			t.Errorf("sentinel %q: expected disk-only (empty table_name), got %q", sentinel, tn)
		}
	}
}

// TestUpdateUpload_NoneDetachesTable: an endpoint bound to a real table can be
// switched to disk-only by updating table_name to "none". This is the only way
// to unbind after create (empty means "leave unchanged").
func TestUpdateUpload_NoneDetachesTable(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustCreateUploadTable(t, db, "files_tbl")
	if _, err := epCreateUpload(ctx, "files", "files_tbl", false, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if tn := endpointTableName(t, db, "files"); tn != "files_tbl" {
		t.Fatalf("precondition: expected binding to files_tbl, got %q", tn)
	}

	res, err := epUpdateUpload(ctx, "files", "none", nil, 0, nil, 0, "")
	if err != nil || res == nil || !res.Success {
		t.Fatalf("detach update: err=%v res=%+v", err, res)
	}
	if tn := endpointTableName(t, db, "files"); tn != "" {
		t.Errorf("expected detached (empty table_name), got %q", tn)
	}
}

// TestUpdateUpload_RejectsNonexistentTable: the existence check also guards the
// update path.
func TestUpdateUpload_RejectsNonexistentTable(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := epCreateUpload(ctx, "files", "", false, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, _ := epUpdateUpload(ctx, "files", "ghost", nil, 0, nil, 0, "")
	if res.Success {
		t.Fatal("expected rejection re-pointing to a non-existent table")
	}
	if !strings.Contains(res.Error, "does not exist") {
		t.Errorf("expected 'does not exist' in error, got %q", res.Error)
	}
}

// TestCreateUpload_RejectsUnfillableShareCode: a NOT NULL, no-default column
// beyond the four auto-filled ones (here a share `code`) makes every upload fail
// the constraint, so the bind is rejected and the column is named back to the
// caller. See uploads_unfillable_test.go for the full contract.
func TestCreateUpload_RejectsUnfillableShareCode(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	_, err := db.Exec(`CREATE TABLE files (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		filename TEXT NOT NULL,
		url TEXT,
		content_type TEXT,
		size INTEGER
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	res, err := epCreateUpload(ctx, "files", "files", false, 0, nil, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection: every upload would fail the code constraint")
	}
	data, _ := res.Data.(map[string]any)
	cols, _ := data["unfillable_required_columns"].([]string)
	if len(cols) != 1 || cols[0] != "code" {
		t.Fatalf("expected unfillable_required_columns=[code], got %v", data["unfillable_required_columns"])
	}
	// A share code is server-generated, so `generate` is the fix to point at.
	if !strings.Contains(res.Error, "generate") {
		t.Errorf("expected the error to name generate, got %q", res.Error)
	}
}

// TestCreateUpload_NoRejectionWhenSatisfiable: a table whose only extra columns
// are nullable (or have defaults) binds cleanly.
func TestCreateUpload_NoRejectionWhenSatisfiable(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	_, err := db.Exec(`CREATE TABLE files (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		filename TEXT NOT NULL,
		url TEXT,
		content_type TEXT,
		size INTEGER,
		created_at TEXT DEFAULT CURRENT_TIMESTAMP,
		note TEXT
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	res, err := epCreateUpload(ctx, "files", "files", false, 0, nil, 0, "")
	if err != nil || !res.Success {
		t.Fatalf("expected success, got err=%v res=%+v", err, res)
	}
	data, _ := res.Data.(map[string]any)
	if _, ok := data["unfillable_required_columns"]; ok {
		t.Errorf("did not expect unfillable columns, got %v", data["unfillable_required_columns"])
	}
}
