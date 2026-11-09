package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
)

// A filename carrying ".." segments or an absolute path must be rejected before
// any directory is created, so a caller can never write outside the project's
// storage dir. Regression test for the WriteFileToDisk containment guard.
func TestWriteFileToDisk_RejectsTraversal(t *testing.T) {
	bad := []string{
		"../secrets.key",
		"../../etc/passwd",
		"a/../../b",
		"sub/../../../escape",
		`..\..\windows`,
	}
	dataDir := t.TempDir()
	for _, name := range bad {
		got, err := WriteFileToDisk(dataDir, 999999, "assets", name, []byte("x"))
		if err == nil {
			t.Fatalf("filename %q: expected rejection, got path %q", name, got)
		}
		if !strings.Contains(err.Error(), "invalid filename") {
			t.Fatalf("filename %q: expected an invalid-filename error, got: %v", name, err)
		}
	}
	// An absolute path is rejected too. t.TempDir() is absolute on every OS
	// (drive-lettered on Windows, "/"-rooted on Unix), so this stays portable.
	abs := filepath.Join(t.TempDir(), "evil")
	if _, err := WriteFileToDisk(dataDir, 999999, "assets", abs, []byte("x")); err == nil {
		t.Fatalf("absolute filename %q: expected rejection", abs)
	}
}

// A plain filename and a legitimately-nested "sub/dir/file" name must pass the
// containment check (they clean to themselves), so the guard doesn't break
// normal writes. Writes land under the passed dataDir, so nothing touches the
// source tree.
func TestWriteFileToDisk_AllowsSafeNames(t *testing.T) {
	dataDir := t.TempDir()
	for _, name := range []string{"api-docs.json", "images/logo.png"} {
		got, err := WriteFileToDisk(dataDir, 1, "assets", name, []byte("hello"))
		if err != nil {
			t.Fatalf("filename %q: unexpected rejection: %v", name, err)
		}
		if !strings.HasSuffix(filepath.ToSlash(got), "assets/"+name) {
			t.Fatalf("filename %q: path %q should end with assets/%s", name, got, name)
		}
	}
}

// A write must land under the configured data dir. The server reads these files
// back through ProjectDBManager.ProjectDir, which is built from the same
// configured dir, so a path that ignores dataDir is written where the reader will
// not look for it and is rejected on read as escaping the project directory.
func TestWriteFileToDisk_HonoursDataDir(t *testing.T) {
	dataDir := t.TempDir()
	got, err := WriteFileToDisk(dataDir, 7, "assets", "logo.png", []byte("hello"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dataDir, "projects", "7", "assets", "logo.png")
	if got != want {
		t.Errorf("write path = %q, want %q (the configured data dir was ignored)", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("file was not written under the configured data dir: %v", err)
	}
}

// StorageDir is the single home for the tool-side path layout; pin that it and
// db.ProjectDBManager.ProjectDir agree on the shape {dataDir}/projects/{id}.
func TestStorageDir_HonoursDataDir(t *testing.T) {
	dataDir := t.TempDir()
	got, err := StorageDir(dataDir, 42, "files")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dataDir, "projects", "42", "files")
	if got != want {
		t.Errorf("StorageDir = %q, want %q", got, want)
	}
}

// StorageDir (writer) and ProjectDBManager.ProjectDir (reader) are two packages
// computing one layout: if they ever disagree, every tool-written file is
// rejected on read - only under a non-default data dir. Pin that they agree.
func TestStorageDirAgreesWithProjectDir(t *testing.T) {
	dataDir := t.TempDir()
	const projectID = 3

	got, err := StorageDir(dataDir, projectID, "files")
	if err != nil {
		t.Fatalf("StorageDir: %v", err)
	}
	readerPrefix := filepath.Clean(db.NewProjectDBManager(dataDir).ProjectDir(projectID))
	if !strings.HasPrefix(filepath.Clean(got), readerPrefix) {
		t.Errorf("StorageDir returned %q, which is outside the reader's project dir %q:\n"+
			"the tool write path and the server read check disagree about the layout", got, readerPrefix)
	}
}
