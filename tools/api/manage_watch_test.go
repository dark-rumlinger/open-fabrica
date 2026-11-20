package api

import (
	"os"
	"path/filepath"
	"testing"

	"openfabrica/tools/tooltest"
)

func TestManageWatch_Lifecycle(t *testing.T) {
	h := tooltest.New(t)

	res := h.MustOK("manage_watch", map[string]any{
		"action":        "add",
		"name":          "csv_imports",
		"glob":          "*.csv,*.json",
		"function_path": "on-csv-drop",
		"poll_sec":      5,
	})
	data := res.Data.(map[string]any)
	folder, _ := data["folder"].(string)
	if folder == "" {
		t.Fatal("expected resolved drop-zone folder path")
	}
	if fi, err := os.Stat(folder); err != nil || !fi.IsDir() {
		t.Fatalf("expected drop-zone folder to be created on disk: %q err=%v", folder, err)
	}

	// Protected credential path is rejected.
	h.MustFail("manage_watch", map[string]any{
		"action": "add",
		"name":   "bad_sec",
		"folder": filepath.Join(t.TempDir(), ".security"),
	}, "")

	listRes := h.MustOK("manage_watch", map[string]any{"action": "list"})
	listData := listRes.Data.(map[string]any)
	if count, _ := listData["count"].(int); count != 1 {
		t.Fatalf("expected 1 watcher, got %+v", listData)
	}

	h.MustOK("manage_watch", map[string]any{
		"action": "delete",
		"name":   "csv_imports",
	})
}
