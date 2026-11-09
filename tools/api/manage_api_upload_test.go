package api

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/internal/endpoints"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupUploadDB is a migrated project database: every of_ table exactly as production
// has it, so a test cannot pass against a hand-written schema that has drifted.
func setupUploadDB(t *testing.T) dbh.Handle {
	return tooltest.ProjectDB(t)
}

func loadEndpoint(t *testing.T, db dbh.Handle, path string) (allowedTypes []string, maxSizeMB int) {
	t.Helper()
	var raw string
	if err := db.QueryRow("SELECT allowed_types, max_size_mb FROM of_upload_endpoints WHERE path = ?", path).Scan(&raw, &maxSizeMB); err != nil {
		t.Fatalf("load endpoint: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &allowedTypes); err != nil {
		t.Fatalf("decode allowed_types: %v", err)
	}
	return
}

func TestCreateUpload_AppliesDefaultsWhenOmitted(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := epCreateUpload(ctx, "docs", "", false, 0, nil, 0, "")
	if err != nil || !res.Success {
		t.Fatalf("expected success, got err=%v res=%+v", err, res)
	}
	types, size := loadEndpoint(t, db, "docs")
	if size != 5 {
		t.Errorf("expected default 5 MB, got %d", size)
	}
	if len(types) != len(defaultUploadAllowedTypes) {
		t.Errorf("expected default allowed_types, got %v", types)
	}
}

func TestCreateUpload_ClampsMaxSizeAboveAbsoluteCap(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := epCreateUpload(ctx, "huge", "", false, 0, nil, 10000, "")
	if err != nil || !res.Success {
		t.Fatalf("expected success, got err=%v res=%+v", err, res)
	}
	_, size := loadEndpoint(t, db, "huge")
	if size != endpoints.MaxUploadSizeMB {
		t.Errorf("expected clamp to %d, got %d", endpoints.MaxUploadSizeMB, size)
	}
}

func TestCreateUpload_RejectsDangerousMIME(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := epCreateUpload(ctx, "exe", "", false, 0, []any{"application/x-msdownload"}, 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection of executable MIME type")
	}
	if !strings.Contains(strings.ToLower(res.Error), "not allowed") {
		t.Errorf("expected 'not allowed' in error, got %q", res.Error)
	}
}

func TestCreateUpload_RejectsExecutableGlob(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := epCreateUpload(ctx, "exe2", "", false, 0, []any{"application/some-executable"}, 0, "")
	if res.Success {
		t.Fatal("expected rejection of pattern containing 'executable'")
	}
}

func TestUpdateUpload_ClampsAndValidates(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	if _, err := epCreateUpload(ctx, "media", "", false, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, _ := epUpdateUpload(ctx, "media", "", nil, 0, []any{"video/*"}, 10000, "")
	if !res.Success {
		t.Fatalf("update: %s", res.Error)
	}
	types, size := loadEndpoint(t, db, "media")
	if size != endpoints.MaxUploadSizeMB {
		t.Errorf("expected clamp to %d, got %d", endpoints.MaxUploadSizeMB, size)
	}
	if len(types) != 1 || types[0] != "video/*" {
		t.Errorf("expected [video/*], got %v", types)
	}

	res, _ = epUpdateUpload(ctx, "media", "", nil, 0, []any{"application/x-sh"}, 0, "")
	if res.Success {
		t.Fatal("expected rejection of shell MIME on update")
	}
}

// TestUpdateUpload_TableNameRoundTrip pins the arg-mismatch fix: the update
// action declared table_name but the handler ignored it, so re-pointing an
// upload endpoint at a new table silently no-oped.
func TestUpdateUpload_TableNameRoundTrip(t *testing.T) {
	db := setupUploadDB(t)
	ctx := &tools.ToolContext{DB: db}
	// The linked tables must exist - create/update now reject a table_name
	// that doesn't resolve to a real table.
	mustCreateUploadTable(t, db, "uploads_a")
	mustCreateUploadTable(t, db, "uploads_b")
	if _, err := epCreateUpload(ctx, "files", "uploads_a", false, 0, nil, 0, ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	res, _ := epUpdateUpload(ctx, "files", "uploads_b", nil, 0, nil, 0, "")
	if res == nil || !res.Success {
		t.Fatalf("update: %+v", res)
	}

	var tableName string
	if err := db.QueryRow("SELECT COALESCE(table_name,'') FROM of_upload_endpoints WHERE path = ?", "files").Scan(&tableName); err != nil {
		t.Fatalf("load endpoint: %v", err)
	}
	if tableName != "uploads_b" {
		t.Fatalf("table_name not updated: %q", tableName)
	}
}
