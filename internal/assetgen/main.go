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
	"regexp"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"

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

var cssLinkRe = regexp.MustCompile(`(?i)<link\s+rel="stylesheet"\s+href="(/css/[^"]+\.css)"\s*>`)

// plan computes every generated file's desired content, keyed by its repo-relative
// slash path. The working directory must be the repo root.
//
// In addition to minifying, plan bundles web/admin's 30 CSS stylesheets into one
// /css/app.css and bundles web/admin/app/main.js via esbuild (with ESM splitting
// so locale catalogs remain lazy-loaded chunks), eliminating the ~180-request
// HTTP/1.1 waterfall on admin cold load while keeping plaintext web/admin/ for dev.
func plan() (map[string][]byte, error) {
	out := map[string][]byte{}

	// 1. Read web/admin/index.html, bundle all referenced /css/*.css in exact
	//    declaration order into web/admin-min/css/app.css, and rewrite index.html
	//    to load that single stylesheet.
	indexRaw, err := os.ReadFile(filepath.Join(adminSrc, "index.html"))
	if err != nil {
		return nil, fmt.Errorf("reading admin index.html: %w", err)
	}
	indexHTML := string(indexRaw)
	var cssBuf strings.Builder
	var cssReadErr error
	bundledCSS := map[string]bool{}
	replacedFirst := false
	rewrittenHTML := cssLinkRe.ReplaceAllStringFunc(indexHTML, func(tag string) string {
		m := cssLinkRe.FindStringSubmatch(tag)
		if len(m) < 2 {
			return tag
		}
		href := strings.TrimPrefix(m[1], "/")
		bundledCSS[href] = true
		raw, rerr := os.ReadFile(filepath.Join(adminSrc, filepath.FromSlash(href)))
		if rerr != nil {
			if cssReadErr == nil {
				cssReadErr = fmt.Errorf("reading %s: %w", href, rerr)
			}
			return tag
		}
		cssBuf.Write(raw)
		cssBuf.WriteByte('\n')
		if !replacedFirst {
			replacedFirst = true
			return `<link rel="stylesheet" href="/css/app.css">`
		}
		return ""
	})
	if cssReadErr != nil {
		return nil, cssReadErr
	}
	if cssBuf.Len() > 0 {
		minCSS, err := assetmin.MinifyCSS("web/admin/css/app.css", cssBuf.String())
		if err != nil {
			return nil, fmt.Errorf("minifying bundled admin CSS: %w", err)
		}
		out[filepath.ToSlash(filepath.Join(adminOut, "css", "app.css"))] = []byte(minCSS)
	}
	out[filepath.ToSlash(filepath.Join(adminOut, "index.html"))] = []byte(assetmin.MinifyHTML(rewrittenHTML))

	// 2. Bundle web/admin/app/main.js with esbuild (ESM splitting keeps dynamic
	//    locale imports as separate lazy chunks).
	absRoot, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	res := api.Build(api.BuildOptions{
		EntryPoints:       []string{filepath.Join(adminSrc, "app", "main.js")},
		Bundle:            true,
		Splitting:         true,
		Format:            api.FormatESModule,
		Target:            api.ES2022,
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		Outdir:            filepath.Join(adminOut, "app"),
		ChunkNames:        "chunks/[name]-[hash]",
		Write:             false,
	})
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("bundling admin JS: %s", res.Errors[0].Text)
	}
	for _, f := range res.OutputFiles {
		rel, err := filepath.Rel(absRoot, f.Path)
		if err != nil {
			return nil, err
		}
		out[filepath.ToSlash(rel)] = f.Contents
	}

	// 3. Minify classic pre-paint theme-boot.js separately and copy static assets
	//    (fonts, icons, any unbundled files).
	themeRaw, err := os.ReadFile(filepath.Join(adminSrc, "app", "theme-boot.js"))
	if err != nil {
		return nil, fmt.Errorf("reading theme-boot.js: %w", err)
	}
	themeMin, err := assetmin.MinifyJS("web/admin/app/theme-boot.js", string(themeRaw), true)
	if err != nil {
		return nil, err
	}
	out[filepath.ToSlash(filepath.Join(adminOut, "app", "theme-boot.js"))] = []byte(themeMin)

	err = filepath.WalkDir(adminSrc, func(path string, d fs.DirEntry, err error) error {
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
		relSlash := filepath.ToSlash(rel)
		if relSlash == "index.html" || strings.HasPrefix(relSlash, "app/") || bundledCSS[relSlash] {
			return nil
		}
		// #nosec G122 -- build-time tool walking the repo's own admin source tree, not untrusted input.
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
