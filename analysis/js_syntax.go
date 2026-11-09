// Package analysis statically analyzes project JS, CSS, and HTML for the
// write gates, the check action, and the post-build validators.
package analysis

// Shared JavaScript syntax check. One esbuild-backed parser so every consumer -
// the post-edit write gate and the build-time readiness validator - agrees on
// what "this module parses" means. Living in analysis (a leaf package) lets both
// the content tools and the build-time validator use it without an import cycle.

import (
	"fmt"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// JSSyntaxReport is the parse result for a code artifact. Gated is false for any
// path the syntax check does not cover (everything but the JS family); callers
// skip the check then. When Gated is true, Clean reports whether the body parsed
// without error; on a failure ErrorCount holds the number of syntax errors,
// FirstLine the 1-based line of the first one, and Detail a short
// "message (line:col)" describing it.
type JSSyntaxReport struct {
	Gated      bool
	Clean      bool
	ErrorCount int
	FirstLine  int
	Detail     string
}

// jsSyntaxLoader returns the esbuild loader for a code path and whether the
// syntax check covers it at all. Only the JavaScript family is gated: a parse
// error there reliably means a module that throws on load. CSS parsers recover
// from most problems rather than failing, so stylesheets pass through unchecked.
func jsSyntaxLoader(p string) (api.Loader, bool) {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs", ".cjs":
		return api.LoaderJS, true
	case ".jsx":
		return api.LoaderJSX, true
	}
	return api.LoaderNone, false
}

// JSSyntax parses body as the module type implied by p and reports its syntax
// health. Only parsing is performed (no minify or target lowering) so the result
// reflects genuine syntax errors, not transform limits; top-level import/export
// are accepted, matching how these files are served as ES modules. A non-JS path
// returns {Gated: false}.
func JSSyntax(p, body string) JSSyntaxReport {
	loader, ok := jsSyntaxLoader(p)
	if !ok {
		return JSSyntaxReport{}
	}
	res := api.Transform(body, api.TransformOptions{Loader: loader})
	if len(res.Errors) == 0 {
		return JSSyntaxReport{Gated: true, Clean: true}
	}
	r := JSSyntaxReport{Gated: true, ErrorCount: len(res.Errors)}
	m := res.Errors[0]
	if m.Location != nil {
		r.FirstLine = m.Location.Line
		r.Detail = fmt.Sprintf("%s (%d:%d)", m.Text, m.Location.Line, m.Location.Column)
	} else {
		r.Detail = m.Text
	}
	return r
}
