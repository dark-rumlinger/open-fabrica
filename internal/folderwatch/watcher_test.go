package folderwatch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestValidateWatchFolder(t *testing.T) {
	projDir := t.TempDir()

	// Default empty folder resolves under <projectDir>/watch/<name>.
	got, err := ValidateWatchFolder(projDir, "", "receipts")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(projDir, "watch", "receipts")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Relative traversal escapes watch root -> blocked.
	if _, err := ValidateWatchFolder(projDir, "../../etc", "bad"); err == nil {
		t.Fatal("expected error for escaping relative path")
	}

	// Sensitive credential / system paths -> blocked.
	for _, bad := range []string{
		filepath.Join(projDir, ".security"),
		filepath.Join(projDir, ".ssh"),
	} {
		if _, err := ValidateWatchFolder(projDir, bad, "bad"); err == nil {
			t.Errorf("expected blocked error for %q", bad)
		}
	}
}

func TestHub_DetectsDroppedFileOnce(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	seen := make(map[string]bool)
	gotCh := make(chan FileEvent, 4)

	hub := NewHub(ProjectHooks{
		LoadWatchers: func(projectID int) ([]WatchSpec, error) {
			return []WatchSpec{{
				Name:    "csv_drop",
				Folder:  dir,
				Glob:    "*.csv,*.txt",
				PollSec: 2,
			}}, nil
		},
		IsSeen: func(projectID int, watcherName, relPath string, size, modTimeNs int64) bool {
			mu.Lock()
			defer mu.Unlock()
			return seen[relPath]
		},
		MarkSeen: func(projectID int, watcherName, relPath string, size, modTimeNs int64) {
			mu.Lock()
			seen[relPath] = true
			mu.Unlock()
		},
		OnFile: func(projectID int, ev FileEvent) {
			gotCh <- ev
		},
	})
	defer hub.CloseAll()

	// Drop a matching CSV and a non-matching BIN file.
	if err := os.WriteFile(filepath.Join(dir, "orders.csv"), []byte("id,total\n1,42.50\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignore.bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}

	hub.Reload(1)

	select {
	case ev := <-gotCh:
		if ev.Name != "orders.csv" || !ev.IsText || ev.Content != "id,total\n1,42.50\n" {
			t.Fatalf("unexpected FileEvent: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dropped CSV event")
	}
}
