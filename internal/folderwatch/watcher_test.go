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
		if ev.Name != "orders.csv" || !ev.IsText || ev.Content != "id,total\n1,42.50\n" || ev.Event != "created" || ev.Kind != "created" {
			t.Fatalf("unexpected FileEvent: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dropped CSV event")
	}
}

func TestHub_SubdirsModifiedAndDeleted(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	seen := make(map[string]SeenFile)
	gotCh := make(chan FileEvent, 16)

	hub := NewHub(ProjectHooks{
		LoadWatchers: func(projectID int) ([]WatchSpec, error) {
			return []WatchSpec{{
				Name:    "dropwatch",
				Folder:  dir,
				Glob:    "*",
				PollSec: 2,
			}}, nil
		},
		ListSeen: func(projectID int, watcherName string) (map[string]SeenFile, error) {
			mu.Lock()
			defer mu.Unlock()
			out := make(map[string]SeenFile, len(seen))
			for k, v := range seen {
				out[k] = v
			}
			return out, nil
		},
		MarkSeen: func(projectID int, watcherName, relPath string, size, modTimeNs int64) {
			mu.Lock()
			seen[relPath] = SeenFile{SizeBytes: size, ModTimeNs: modTimeNs}
			mu.Unlock()
		},
		DeleteSeen: func(projectID int, watcherName, relPath string) {
			mu.Lock()
			delete(seen, relPath)
			mu.Unlock()
		},
		OnFile: func(projectID int, ev FileEvent) {
			gotCh <- ev
		},
	})
	defer hub.CloseAll()

	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sub, "note.txt")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Initial scan -> created event with relative path "nested/note.txt"
	hub.scanOne(t.Context(), 1, WatchSpec{Name: "dropwatch", Folder: dir, Glob: "*"})
	select {
	case ev := <-gotCh:
		if ev.Event != "created" || ev.Kind != "created" || ev.RelPath != "nested/note.txt" || ev.Content != "v1" {
			t.Fatalf("expected created nested/note.txt, got %+v", ev)
		}
	default:
		t.Fatal("expected created event")
	}

	// 2. Modify file -> modified/changed event
	if err := os.WriteFile(target, []byte("version-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub.scanOne(t.Context(), 1, WatchSpec{Name: "dropwatch", Folder: dir, Glob: "*"})
	select {
	case ev := <-gotCh:
		if ev.Event != "modified" || ev.Kind != "changed" || ev.RelPath != "nested/note.txt" || ev.Content != "version-2" {
			t.Fatalf("expected modified nested/note.txt, got %+v", ev)
		}
	default:
		t.Fatal("expected modified event")
	}

	// 3. ListWatchDir returns both "nested" (is_dir=true) and "nested/note.txt"
	entries, avail, err := ListWatchDir(dir, "*")
	if err != nil || !avail || len(entries) != 2 {
		t.Fatalf("ListWatchDir got len=%d avail=%v err=%v", len(entries), avail, err)
	}

	// 4. Delete file -> deleted event
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	hub.scanOne(t.Context(), 1, WatchSpec{Name: "dropwatch", Folder: dir, Glob: "*"})
	select {
	case ev := <-gotCh:
		if ev.Event != "deleted" || ev.Kind != "deleted" || ev.RelPath != "nested/note.txt" {
			t.Fatalf("expected deleted nested/note.txt, got %+v", ev)
		}
	default:
		t.Fatal("expected deleted event")
	}
}
