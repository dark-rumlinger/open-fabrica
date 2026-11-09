package diskpath

import (
	"path/filepath"
	"testing"
)

func TestInsideRespectsSegmentBoundary(t *testing.T) {
	dir := filepath.Join("data", "projects", "1")
	if !Inside(dir, filepath.Join(dir, "files", "a.png")) {
		t.Error("a file under the project must be inside")
	}
	if Inside(dir, filepath.Join("data", "projects", "10", "files", "a.png")) {
		t.Error("project 10 must not count as inside project 1")
	}
	if Inside(dir, filepath.Join(dir, "..", "2", "x")) {
		t.Error("a dot-dot escape must not be inside")
	}
}

func TestRebase(t *testing.T) {
	newDir := filepath.Join("data", "projects", "12")
	cases := map[string]string{
		"data/projects/7/files/logo.png":             "data/projects/12/files/logo.png",
		`data\projects\7\files\logo.png`:             "data/projects/12/files/logo.png",
		"/srv/of/data/projects/7/assets/hero.jpg":    "data/projects/12/assets/hero.jpg",
		`C:\of\data\projects\7\files\x.png`:          "data/projects/12/files/x.png",
		"/home/me/projects/7/of/data/projects/7/f/a": "data/projects/12/f/a",
		"somewhere/else/x.png":                       "somewhere/else/x.png",
		"data/projects/99/files/x.png":               "data/projects/99/files/x.png",
		"data/projects/7/../../../etc/passwd":        "data/projects/7/../../../etc/passwd",
	}
	for in, want := range cases {
		if got := filepath.ToSlash(Rebase(in, 7, newDir)); got != want {
			t.Errorf("Rebase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	dir := filepath.Join("data", "projects", "1")
	if got, ok := Resolve(dir, filepath.Join(dir, "files", "a.png")); !ok || got != filepath.Join(dir, "files", "a.png") {
		t.Errorf("in-place path: %q %v", got, ok)
	}
	// The data directory moved: an absolute path from the old location re-anchors.
	if got, ok := Resolve(dir, "/old/place/data/projects/1/files/a.png"); !ok || filepath.ToSlash(got) != "data/projects/1/files/a.png" {
		t.Errorf("moved data dir: %q %v", got, ok)
	}
	if _, ok := Resolve(dir, filepath.Join("data", "projects", "10", "files", "a.png")); ok {
		t.Error("another project's file must be refused")
	}
	if _, ok := Resolve(dir, "/etc/passwd"); ok {
		t.Error("an unrelated path must be refused")
	}
	if _, ok := Resolve(dir, "/x/projects/1/../../2/secret"); ok {
		t.Error("a dot-dot escape under the marker must be refused")
	}
}
