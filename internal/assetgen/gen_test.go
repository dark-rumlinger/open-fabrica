package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"openfabrica/internal/repopath"
)

// TestGeneratedAssetsMatchSources guards against an admin/ or helper source edit
// that skipped regeneration: it recomputes the intended minified output and compares
// it to what is on disk (and embedded), and checks that no stale minified file
// survives. Fix a failure by running `go generate ./web`.
func TestGeneratedAssetsMatchSources(t *testing.T) {
	root, err := repopath.Root()
	if err != nil {
		t.Fatalf("locating repo root: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir repo root: %v", err)
	}

	outputs, err := plan()
	if err != nil {
		t.Fatalf("planning assets: %v", err)
	}
	if len(outputs) == 0 {
		t.Fatal("no generated assets planned - is web/admin present?")
	}

	for relslash, want := range outputs {
		got, err := os.ReadFile(filepath.FromSlash(relslash))
		if err != nil {
			t.Errorf("%s missing - run `go generate ./web`: %v", relslash, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale - run `go generate ./web`", relslash)
		}
	}

	// No stale minified file: everything under admin-min must be in the plan.
	err = filepath.WalkDir(adminOut, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, ok := outputs[filepath.ToSlash(path)]; !ok {
			t.Errorf("%s has no source - run `go generate ./web`", filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", adminOut, err)
	}
}
