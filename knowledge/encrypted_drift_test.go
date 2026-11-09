package knowledge

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The wrapper must satisfy the fs interfaces callers reach it through, both the
// concrete-method callers (contracts.load, engine tests) and the fs.* helpers
// (fs.WalkDir, fs.ReadFile, fs.Stat, pack.ParseFS).
var (
	_ fs.FS         = FS
	_ fs.ReadFileFS = FS
	_ fs.ReadDirFS  = FS
	_ fs.StatFS     = FS
)

// TestEncryptedTreeMatchesPlaintext guards against a plaintext edit that skipped
// regeneration: it decrypts every embedded file and compares it to the plaintext
// source on disk, and checks that no stale ciphertext survives without a source.
// Fix a failure by running `go generate ./knowledge`.
func TestEncryptedTreeMatchesPlaintext(t *testing.T) {
	roots := []string{"mental-model.md", "guides", "stages", "contracts"}
	seen := map[string]bool{}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			t.Fatalf("plaintext root %s missing: %v", root, err)
		}
		if !info.IsDir() {
			checkFile(t, root, seen)
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			checkFile(t, p, seen)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	err := fs.WalkDir(FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !seen[p] {
			t.Errorf("embedded %s has no plaintext source - run `go generate ./knowledge`", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded FS: %v", err)
	}
}

func checkFile(t *testing.T, diskPath string, seen map[string]bool) {
	t.Helper()
	virtual := filepath.ToSlash(diskPath) // fs paths are always slash-separated
	seen[virtual] = true
	want, err := os.ReadFile(diskPath)
	if err != nil {
		t.Errorf("reading plaintext %s: %v", diskPath, err)
		return
	}
	got, err := FS.ReadFile(virtual)
	if err != nil {
		t.Errorf("decrypting embedded %s (run `go generate ./knowledge`): %v", virtual, err)
		return
	}
	if !bytes.Equal(got, want) {
		t.Errorf("decrypted %s does not match plaintext - run `go generate ./knowledge`", virtual)
	}
}
