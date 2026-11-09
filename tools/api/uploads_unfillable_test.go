package api

import (
	"strings"
	"testing"

	"openfabrica/tools"

	_ "modernc.org/sqlite"
)

// TestCreateUpload_RejectsUnfillableNotNullColumn: a NOT NULL, no-default column
// outside the four the upload fills makes EVERY upload fail the constraint, so
// the endpoint can never work. Reject the bind instead of committing a live
// endpoint that is guaranteed to 500 - the same treatment a non-existent table
// already gets.
func TestCreateUpload_RejectsUnfillableNotNullColumn(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`CREATE TABLE dj_tracks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		dj_name TEXT NOT NULL,
		track_title TEXT NOT NULL,
		filename TEXT NOT NULL,
		url TEXT,
		content_type TEXT,
		size INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	res, err := epCreateUpload(ctx, "dj-tracks", "dj_tracks", false, 0, nil, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection binding to a table with unfillable NOT NULL columns")
	}

	// Nothing may commit: no endpoint row, no spine claim.
	var eps int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_upload_endpoints WHERE path = 'dj-tracks'").Scan(&eps)
	if eps != 0 {
		t.Errorf("expected no of_upload_endpoints row, got %d", eps)
	}
	var claims int
	_ = db.QueryRow("SELECT COUNT(*) FROM of_endpoints WHERE path = 'dj-tracks'").Scan(&claims)
	if claims != 0 {
		t.Errorf("expected no of_endpoints spine claim, got %d", claims)
	}
}

// TestCreateUpload_UnfillableErrorNamesReachableRemedies: the message must name
// only moves the caller can actually make. "Make the column nullable" is not one
// of them - alter_table has no modify-column path - so a caller following that
// instruction has nowhere to go but drop_table. The remedies that exist are
// detaching to disk-only and creating the row through the table's own create_api
// POST, or declaring the column with generate.
func TestCreateUpload_UnfillableErrorNamesReachableRemedies(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`CREATE TABLE dj_tracks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		dj_name TEXT NOT NULL,
		track_title TEXT NOT NULL,
		filename TEXT NOT NULL,
		url TEXT,
		content_type TEXT,
		size INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	res, _ := epCreateUpload(ctx, "dj-tracks", "dj_tracks", false, 0, nil, 0, "")
	if res.Success {
		t.Fatal("expected rejection")
	}

	if strings.Contains(strings.ToLower(res.Error), "nullable") {
		t.Errorf("error must not prescribe making the column nullable (alter_table cannot): %q", res.Error)
	}
	for _, want := range []string{"dj_name", "track_title", "none", "create_api", "generate"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("expected error to mention %q, got %q", want, res.Error)
		}
	}
}

// TestCreateUpload_GeneratedColumnBindsCleanly: a NOT NULL column carrying a
// generate expression has a DEFAULT, so SQLite fills it on the upload's insert.
// The rejection above must not fire for it.
func TestCreateUpload_GeneratedColumnBindsCleanly(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := db.Exec(`CREATE TABLE files (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE DEFAULT (lower(hex(randomblob(4)))),
		filename TEXT NOT NULL,
		url TEXT,
		content_type TEXT,
		size INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	res, err := epCreateUpload(ctx, "files", "files", false, 0, nil, 0, "")
	if err != nil || !res.Success {
		t.Fatalf("generated column must bind cleanly, got err=%v res=%+v", err, res)
	}
}

// TestUpdateUpload_RejectsUnfillableNotNullColumn: rebinding through update
// reaches the same unusable state as creating, so it gets the same check, and a
// rejected update leaves the existing binding untouched.
func TestUpdateUpload_RejectsUnfillableNotNullColumn(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	mustCreateUploadTable(t, db, "good_tbl")
	if _, err := db.Exec(`CREATE TABLE bad_tbl (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		caption TEXT NOT NULL,
		filename TEXT NOT NULL,
		url TEXT,
		content_type TEXT,
		size INTEGER
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := epCreateUpload(ctx, "files", "good_tbl", false, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := epUpdateUpload(ctx, "files", "bad_tbl", nil, 0, nil, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection re-pointing to a table with unfillable NOT NULL columns")
	}
	if tn := endpointTableName(t, db, "files"); tn != "good_tbl" {
		t.Errorf("binding must be unchanged after a rejected update, got %q", tn)
	}
}
