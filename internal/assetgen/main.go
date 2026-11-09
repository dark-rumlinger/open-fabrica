// Command assetgen regenerates the minified admin UI tree (web/admin-min/) from
// the plaintext admin tree (web/admin/), and the minified copies of the injected
// client helpers (auth/e2ee/ofnet .min.js). The plaintext files stay on disk as
// what developers edit and what the dev build serves from disk; only the minified
// copies are embedded in the binary (see web/embed.go and the helper embeds).
//
// It runs from any working directory (it locates the repo root itself), so it
// behaves identically under `go generate`, release.sh and start_dev.bat. Output is
// deterministic, so a re-run with unchanged sources rewrites nothing. The planning
// is split into plan() so a test can compare the on-disk result to the intended one
// and catch a source edit that skipped regeneration.
package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"openfabrica/internal/assetmin"
	"openfabrica/internal/repopath"
)

const (
	adminSrc = "web/admin"
	adminOut = "web/admin-min"
)

// helpers are the injected client helpers: minified without identifier mangling,
// since they ship into generated user projects (keep their exported API and names
// stable and debuggable) and carry emit-time placeholders like __AUTH_PATH__.
var helpers = []struct{ in, out string }{
	{"tools/auth/js/auth.js", "tools/auth/js/auth.min.js"},
	{"tools/api/js/e2ee.js", "tools/api/js/e2ee.min.js"},
	{"tools/api/js/ofnet.js", "tools/api/js/ofnet.min.js"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "assetgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repopath.Root()
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	outputs, err := plan()
	if err != nil {
		return err
	}
	for relslash, data := range outputs {
		if err := writeIfChanged(filepath.FromSlash(relslash), data); err != nil {
			return err
		}
	}
	return pruneAdmin(outputs)
}

// plan computes every generated file's desired content, keyed by its repo-relative
// slash path. The working directory must be the repo root.
func plan() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := filepath.WalkDir(adminSrc, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if assetmin.SkipAsset(path) {
			return nil
		}
		rel, err := filepath.Rel(adminSrc, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		data, err := transform(path, raw)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(filepath.Join(adminOut, rel))] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, h := range helpers {
		raw, err := os.ReadFile(h.in)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", h.in, err)
		}
		min, err := assetmin.MinifyJS(filepath.ToSlash(h.in), string(raw), false)
		if err != nil {
			return nil, err
		}
		out[filepath.ToSlash(h.out)] = []byte(min)
	}
	return out, nil
}

// transform minifies raw by src's extension; unknown types are copied verbatim.
// Admin JS is mangled (its files are ES modules; esbuild keeps exports intact).
func transform(src string, raw []byte) ([]byte, error) {
	name := filepath.ToSlash(src)
	switch strings.ToLower(filepath.Ext(src)) {
	case ".js":
		min, err := assetmin.MinifyJS(name, string(raw), true)
		return []byte(min), err
	case ".css":
		min, err := assetmin.MinifyCSS(name, string(raw))
		return []byte(min), err
	case ".html":
		return []byte(assetmin.MinifyHTML(string(raw))), nil
	default:
		return raw, nil
	}
}

func writeIfChanged(out string, data []byte) error {
	if existing, err := os.ReadFile(out); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	return nil
}

// pruneAdmin removes files under adminOut that plan did not produce, so a deleted
// or renamed source does not leave a stale minified file behind.
func pruneAdmin(outputs map[string][]byte) error {
	if _, err := os.Stat(adminOut); os.IsNotExist(err) {
		return nil
	}
	var stale []string
	err := filepath.WalkDir(adminOut, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, ok := outputs[filepath.ToSlash(path)]; !ok {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(stale)
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("removing stale %s: %w", p, err)
		}
	}
	return nil
}
