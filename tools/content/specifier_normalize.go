package content

// Save-time specifier normalization: the write-path hook over
// analysis.RewriteRelativeSpecifiers. A relative project-module import
// ('./world.state.js') that lands in a stored body diverges from the bare style
// ('world.state') the platform teaches, renders in the snapshot, and maps in
// the importmap - and at runtime the two styles resolve to different URLs, so a
// mixed graph instantiates the module twice and forks its live bindings. The
// gate rewrites what it can prove safe and leaves everything else alone: the
// worst case is always the body as the model wrote it.

import (
	"fmt"

	"openfabrica/analysis"
	"openfabrica/tools"
)

// tryNormalizeSpecifiers rewrites relative project-module specifiers in body to
// their bare logical form. Fires only when the rewrite is provably safe: the
// resolved target maps unambiguously in the post-save module set (a collision
// or a not-yet-saved sibling stays as written, keeping its pending signal), and
// the rewritten body re-passes the syntax gate - else the original body stands
// untouched. Returns the rewritten body, the "'./x.js' -> 'x'" echo lines, and
// ok=true, or ("", nil, false) to keep the original. Deterministic: rewrites
// follow source order, so the same write always yields the same body (the
// replay cache depends on it).
func tryNormalizeSpecifiers(ctx *tools.ToolContext, kind, path, prevBody, body string, exists bool) (string, []string, bool) {
	if kind != "file" || !isJSModulePath(path) {
		return "", nil, false
	}
	bodies := projectJSBodies(ctx, path, body)
	rewritten, rws := analysis.RewriteRelativeSpecifiers(path, body, analysis.ModuleSpecifierIndex(bodies))
	if len(rws) == 0 || rewritten == body {
		return "", nil, false
	}
	if rejectIfBrokeSyntax(kind, path, prevBody, rewritten, exists) != nil {
		return "", nil, false
	}
	echo := make([]string, 0, len(rws))
	seen := map[string]bool{}
	for _, r := range rws {
		line := fmt.Sprintf("'%s' -> '%s'", r.From, r.To)
		if !seen[line] {
			seen[line] = true
			echo = append(echo, line)
		}
	}
	return rewritten, echo, true
}
