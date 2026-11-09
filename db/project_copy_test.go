package db

import (
	"os"
	"path/filepath"
	"testing"

	"openfabrica/internal/diskpath"
)

// A copied project must read its own files: the copy's disk paths name the copy's
// directory, not the source's, or every upload 404s (or reads the original).
func TestCopyFromRelinksDiskPaths(t *testing.T) {
	root := t.TempDir()
	mgr := NewProjectDBManager(root)
	t.Cleanup(mgr.CloseAll)

	src, err := mgr.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(mgr.ProjectDir(1), "files", "logo.png")
	if err := os.MkdirAll(filepath.Dir(srcFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcFile, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ExecWrite(`INSERT INTO of_files (kind, path, storage, disk_path) VALUES ('file', 'logo.png', 'disk', ?)`, srcFile); err != nil {
		t.Fatal(err)
	}

	dst, err := mgr.CopyFrom(1, 2)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	var got string
	if err := dst.Reader().QueryRow(`SELECT disk_path FROM of_files WHERE path = 'logo.png'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !diskpath.Inside(mgr.ProjectDir(2), got) {
		t.Fatalf("copied disk_path %q is not inside the new project dir %q", got, mgr.ProjectDir(2))
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != "png" {
		t.Fatalf("copied file unreadable at %q: %v", got, err)
	}
}

func TestExistsDoesNotCreate(t *testing.T) {
	mgr := NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	if mgr.Exists(5) {
		t.Fatal("an unknown project must not exist")
	}
	if _, err := os.Stat(mgr.ProjectDir(5)); err == nil {
		t.Fatal("Exists must not create the project directory")
	}
	if _, err := mgr.Create(5); err != nil {
		t.Fatal(err)
	}
	if !mgr.Exists(5) {
		t.Fatal("a created project must exist")
	}
	mgr.CloseAll()
	if !mgr.Exists(5) {
		t.Fatal("a project on disk must exist even when not open")
	}
}
