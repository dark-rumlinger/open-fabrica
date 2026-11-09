package content

import (
	"openfabrica/tools"
	"openfabrica/tools/project"
)

// manage_redirects re-exposes the redirect helpers from the project package
// under the content category so the LLM finds them next to page artifacts.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_redirects",
		Category: "content",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Manage project URL redirects (301 permanent / 302 temporary).",
		WhenToUse: `Use to redirect old paths after a rename or reshuffle. Use 301 when the move is
permanent (most cases), 302 when it might revert. Path matching is exact - no
wildcards. For pattern-based rewrites, write the logic in a page or middleware
instead.`,
		Actions: []tools.Action{
			&RedirectAdd{}, &RedirectRemove{}, &RedirectList{},
		},
	})
}

type RedirectAdd struct {
	RedirectFrom string `arg:"redirect_from,required" desc:"Source path (must start with /)."`
	RedirectTo   string `arg:"redirect_to,required"   desc:"Target path or URL."`
	StatusCode   int    `arg:"status_code"            desc:"301 permanent or 302 temporary." default:"301"`
}

func (a *RedirectAdd) Doc() string { return "Add or replace a redirect rule." }
func (a *RedirectAdd) Example() any {
	return &RedirectAdd{RedirectFrom: "/old-blog", RedirectTo: "/blog", StatusCode: 301}
}
func (a *RedirectAdd) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return project.ProjectAddRedirect(ctx, a.RedirectFrom, a.RedirectTo, a.StatusCode)
}

type RedirectRemove struct {
	RedirectFrom string `arg:"redirect_from,required" desc:"Source path to remove."`
}

func (a *RedirectRemove) Doc() string { return "Remove a redirect rule." }
func (a *RedirectRemove) Example() any {
	return &RedirectRemove{RedirectFrom: "/old-blog"}
}
func (a *RedirectRemove) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return project.ProjectRemoveRedirect(ctx, a.RedirectFrom)
}

type RedirectList struct{}

func (a *RedirectList) Doc() string    { return "List every redirect rule." }
func (a *RedirectList) ReadOnly() bool { return true }
func (a *RedirectList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	return project.ProjectListRedirects(ctx)
}
