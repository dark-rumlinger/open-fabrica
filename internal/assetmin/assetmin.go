// Package assetmin minifies admin UI assets (JS, CSS, HTML) at build time using
// esbuild, which is already a dependency. It strips comments and whitespace so the
// shipped binary does not carry the readable, commented source of the admin SPA.
// Minification is deterministic for a given esbuild version, so regenerating an
// unchanged tree produces identical output.
package assetmin

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// moduleRE detects an ES module by a top-level import/export. A file without one
// is treated as a classic script (e.g. admin/app/theme-boot.js, loaded with a
// plain <script src>), where top-level names may be globals other code relies on,
// so its identifiers are left alone.
var moduleRE = regexp.MustCompile(`(?m)^\s*(import|export)[\s{*]`)

// MinifyJS strips comments and whitespace from JavaScript. mangle additionally
// renames local identifiers, but only for ES modules, where the module scope is
// isolated and esbuild never renames the exported bindings other files import.
// For a classic script, or when mangle is false, identifiers are preserved.
func MinifyJS(name, src string, mangle bool) (string, error) {
	isModule := moduleRE.MatchString(src)
	opts := api.TransformOptions{
		Loader:            api.LoaderJS,
		Sourcefile:        name,
		Charset:           api.CharsetUTF8, // keep UTF-8; the default escapes every non-ASCII char and bloats the output
		MinifyWhitespace:  true,
		MinifySyntax:      true,
		MinifyIdentifiers: mangle && isModule,
	}
	if isModule {
		opts.Format = api.FormatESModule
	}
	res := api.Transform(src, opts)
	if len(res.Errors) > 0 {
		return "", esbuildErr(name, res.Errors)
	}
	return string(res.Code), nil
}

// MinifyCSS strips comments and whitespace from CSS.
func MinifyCSS(name, src string) (string, error) {
	res := api.Transform(src, api.TransformOptions{
		Loader:           api.LoaderCSS,
		Sourcefile:       name,
		Charset:          api.CharsetUTF8,
		MinifyWhitespace: true,
		MinifySyntax:     true,
	})
	if len(res.Errors) > 0 {
		return "", esbuildErr(name, res.Errors)
	}
	return string(res.Code), nil
}

// htmlCommentRE matches HTML comments. esbuild has no HTML minifier, so HTML is
// handled conservatively here: only comments are removed (and the blank lines they
// leave behind), never element whitespace, so rendering cannot change.
var htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)

// MinifyHTML removes HTML comments and the resulting blank lines. It deliberately
// does not touch element structure or inline whitespace.
func MinifyHTML(src string) string {
	out := htmlCommentRE.ReplaceAllString(src, "")
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// SkipAsset reports build junk that should never be embedded in the admin tree:
// editor backups (foo~) and image-editor source files (.kra).
func SkipAsset(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, "~") || strings.EqualFold(filepath.Ext(path), ".kra")
}

func esbuildErr(name string, errs []api.Message) error {
	msg := errs[0].Text
	if loc := errs[0].Location; loc != nil {
		return fmt.Errorf("minify %s: %s (%d:%d)", name, msg, loc.Line, loc.Column)
	}
	return fmt.Errorf("minify %s: %s", name, msg)
}
