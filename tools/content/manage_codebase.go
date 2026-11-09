package content

import "openfabrica/tools"

// manage_codebase is the read-only cross-artifact inspection surface:
// whole-project tree, regex grep, and the symbol index (resolve / where_used).
// Single-artifact reads (get, outline, history, version diff) live on
// manage_files.

func init() {
	tools.Define(tools.Tool{
		Name: "manage_codebase",
		// Sized to the tool's own output: the 3000-byte default cut it mid-JSON.
		MaxResult:          12000,
		Category:           "content",
		FullResultInEvents: true,
		OneLiner:           "Search and inspect the project code across artifacts - grep, resolve a symbol, where_used, tree.",
		WhenToUse: `Find where to edit before opening files: resolve a symbol name
(function, CSS selector, HTML id/class, export) straight to file:line + signature +
who references it; where_used lists every reference - both prefer the symbol index
over re-reading candidate files. project_tree gives a compact whole-project view;
grep is the full-text fallback when a name isn't an indexed symbol. For
single-artifact reads (body, outline) and restoring an earlier version, call manage_files.`,
		Actions: []tools.Action{
			&CodeResolve{}, &CodeWhereUsed{}, &CodeTree{}, &CodeGrep{},
		},
	})
}

type CodeResolve struct {
	Symbol string `arg:"symbol,required" desc:"Symbol name to locate: a function, CSS selector, HTML id/class, or export."`
	Kind   string `arg:"kind"            desc:"Optional kind filter (js_func, js_method, js_export, css_selector, html_id, html_class)." enum:"js_func,js_method,js_export,css_selector,html_id,html_class"`
}

func (a *CodeResolve) ActionName() string { return "resolve" }
func (a *CodeResolve) Doc() string {
	return "Locate a symbol's definition(s) - file:line, signature, and who references it - from the symbol index. Prefer this over grep+get to find where to edit."
}
func (a *CodeResolve) ReadOnly() bool { return true }
func (a *CodeResolve) Example() any   { return &CodeResolve{Symbol: "renderHeader"} }
func (a *CodeResolve) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionResolve(ctx, a.Symbol, a.Kind)
}

type CodeWhereUsed struct {
	Symbol string `arg:"symbol,required" desc:"Symbol name to list references for."`
}

func (a *CodeWhereUsed) ActionName() string { return "where_used" }
func (a *CodeWhereUsed) Doc() string {
	return "List every file that references a symbol (imports and calls), grouped by file, from the symbol index."
}
func (a *CodeWhereUsed) ReadOnly() bool { return true }
func (a *CodeWhereUsed) Example() any   { return &CodeWhereUsed{Symbol: "renderHeader"} }
func (a *CodeWhereUsed) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionWhereUsed(ctx, a.Symbol)
}

type CodeTree struct {
	Compact bool `arg:"compact" desc:"Suppress outlines; tree only."`
}

func (a *CodeTree) ActionName() string { return "project_tree" }
func (a *CodeTree) Doc() string {
	return "List every artifact in the project, optionally with outlines."
}
func (a *CodeTree) ReadOnly() bool { return true }
func (a *CodeTree) Example() any   { return &CodeTree{Compact: true} }
func (a *CodeTree) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionProjectTree(ctx, a.Compact)
}

type CodeGrep struct {
	Pattern  string `arg:"pattern,required" desc:"Regex pattern."`
	PathGlob string `arg:"path_glob"        desc:"Limit to artifacts whose path matches this glob (* does not cross /)."`
}

func (a *CodeGrep) ActionName() string { return "grep" }
func (a *CodeGrep) Doc() string        { return "Regex search across artifact bodies." }
func (a *CodeGrep) ReadOnly() bool     { return true }
func (a *CodeGrep) Example() any {
	return &CodeGrep{Pattern: "TODO", PathGlob: "js/*.js"}
}
func (a *CodeGrep) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return artifactActionGrep(ctx, a.Pattern, a.PathGlob)
}
