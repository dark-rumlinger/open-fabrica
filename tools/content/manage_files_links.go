package content

import (
	"fmt"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// Page-asset linking actions for manage_files: bind a page-scope CSS/JS file
// (kind=file, scope=page) to a specific page so it ships only on that page.
// Position orders CSS in cascade order; JS modules resolve through imports.
// Registered in manage_files.go's init alongside the file CRUD actions.
//
// Canonical channel for same-origin page-scope files: save with attach_to, which
// links in the same call. action=attach is the separate-call form, for a file
// already saved or a re-link. Do NOT hand-write <script src="/files/..."> or
// <link rel=stylesheet href="/files/..."> in the page body - the platform
// injects attached files in position order after the page renders.

// FileAttach binds a page-scope file to a page at a given position.
type FileAttach struct {
	FilePath string `arg:"file_path,required" desc:"The file being attached (must already exist). Attaching makes it page-scoped, so a scope=global file is refused while other pages load it."`
	PagePath string `arg:"page_path,required"     desc:"Owning page the file attaches to."`
	Position int    `arg:"position"               desc:"Attach order (default 0, lower first; CSS cascade order - JS modules resolve through imports, not tag order)."`
}

func (a *FileAttach) Doc() string {
	return "Attach an EXISTING page-scope JS/CSS file (kind=file) to a page at a position (lower loads first). The file must already be saved (action=save kind=file) - attaching a path that has not been saved fails. To save and attach in one call, use action=save with attach_to instead."
}
func (a *FileAttach) Example() any {
	return &FileAttach{FilePath: "/files/checkout.js", PagePath: "/checkout"}
}
func (a *FileAttach) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	data, fail := linkFileToPage(ctx, a.FilePath, a.PagePath, a.Position)
	if fail != nil {
		return fail, nil
	}
	return tools.Ok(data), nil
}

// unlinkedSaveNote reports what a page-scope file that just landed still needs to
// reach a browser, or "" when it needs nothing. Called on a kind=file save that
// carried no attach_to, so the gap is named at the point of the omission rather
// than by the next turn's project_state, which arrives too late to fold the fix
// into the save and costs a turn of its own.
//
// Reachability comes from store.UnlinkedPageArtifactsClassified, the same rule
// project_state and the post-build probe use, so the three cannot disagree. It is
// import-aware, which is what makes the note correct across project shapes: a leaf
// module pulled in by a linked entry is not orphaned and gets no note, and a
// project with no page-scope JS or CSS at all (headless, API-only, job-only) has
// an empty orphan set and never reaches the advice.
func unlinkedSaveNote(ctx *tools.ToolContext, filePath string) string {
	if ctx == nil || ctx.DB == nil {
		return ""
	}
	filePath = store.CanonicalFilePath(filePath)
	orphaned := false
	for _, p := range store.UnlinkedPageArtifactsClassified(ctx.DB).Orphans {
		if p == filePath {
			orphaned = true
			break
		}
	}
	if !orphaned {
		return ""
	}
	// Mid-split this is the expected state: keep saving leaves, then attach the
	// entry. The note has to say so, or it reads as "attach every module", which
	// is the wiring mistake the import graph exists to avoid.
	const howToClear = "If it is a leaf another module imports, this clears itself once you save the ENTRY with attach_to - do not attach leaves. " +
		"If it IS the entry (or any CSS, which no module can import), re-save it with attach_to=\"/<route>\", or attach it now."
	if !store.AnyPageExists(ctx.DB) {
		return fmt.Sprintf("`%s` is page-scope but this project has no pages, so nothing can load it. "+
			"Save it with scope=\"global\" if it is sitewide, or create the page it belongs to.", filePath)
	}
	return fmt.Sprintf("`%s` is saved but no page loads it yet. %s "+
		"Passing attach_to on the save does this in one call.", filePath, howToClear)
}

// linkFileToPage performs one page/file attachment: existence checks, the link
// itself, and the page-scope promotion. Shared by the attach action and the
// save action's attach_to shortcut, so both channels behave identically.
// Returns the attachment data on success or a failure Result.
func linkFileToPage(ctx *tools.ToolContext, filePath, pagePath string, position int) (map[string]any, *tools.Result) {
	// attach always names a kind=file artifact, so collapse a /files/ URL form to
	// the bare stored path the file CRUD actions and the served route agree on.
	filePath = store.CanonicalFilePath(filePath)
	if rej := flowScriptWriteGuard(filePath); rej != nil {
		return nil, rej // the test script never ships as a page asset
	}
	if !store.ArtifactExists(ctx.DB, "page", pagePath) {
		return nil, tools.FailCode(tools.CodeNotFound, "page %q does not exist - call manage_files action=save kind=page file_path=%q (body is an HTML fragment, no <!doctype>/<html>/<body>) before attaching files to it", pagePath, pagePath)
	}
	if !store.ArtifactExists(ctx.DB, "file", filePath) {
		return nil, tools.FailCode(tools.CodeNotFound, "file %q does not exist - call manage_files action=save kind=file scope=page file_path=%q (e.g. js/checkout.js or css/styles.css) first, then retry attach.%s", filePath, filePath, store.SuggestArtifactPathsHint(ctx.DB, "file", filePath))
	}
	// Attaching declares a file page-scope, so a file saved GLOBAL and then attached
	// carries two contradictory declarations. Resolving that toward the attach is only
	// free when no other page exists: with one page, global and page ship identically.
	// With others, the demotion takes the file OFF them - a global design system leaves
	// every page but this one unstyled, and nothing downstream reports it. A scope=page
	// file is never in question: attach is exactly what it is for.
	demoteGlobal := store.FileScope(ctx.DB, filePath) == "global"
	if demoteGlobal {
		if others := store.OtherPagePaths(ctx.DB, pagePath); len(others) > 0 {
			return nil, tools.Fail("`%s` is scope=global - it already loads on EVERY page, so it needs no attach to %q. Attaching declares a file page-scope, which would take it off %s and leave %s unstyled, with nothing reporting the breakage. If it is the shared design system, leave it global. If these styles belong to %q alone, save them at their own path with scope=page and attach that, or re-save this file with scope=page first.",
				filePath, pagePath, strings.Join(others, ", "), pluralPages(len(others)), pagePath)
		}
	}
	if err := store.LinkPageAsset(ctx.DB, pagePath, filePath, position); err != nil {
		return nil, tools.FailErr(err)
	}
	data := map[string]any{
		"page_path": pagePath, "file_path": filePath, "position": position,
	}
	if demoteGlobal {
		// Sole page: the file ships identically either way, so correct the scope rather
		// than cost a re-save.
		if promoted, _ := store.PromotePageAssetScope(ctx.DB, filePath); promoted {
			data["hint"] = fmt.Sprintf("set %s scope to \"page\" (it was global); %q is the only page, so it ships identically either way. Add a second page and this file stays on %q alone - save the SHARED design system as a separate scope=global file.", filePath, pagePath, pagePath)
		}
	}
	return data, nil
}

func pluralPages(n int) string {
	if n == 1 {
		return "that page"
	}
	return "those pages"
}

// FileDetach removes a page/file binding.
type FileDetach struct {
	FilePath string `arg:"file_path,required" desc:"The file being detached."`
	PagePath string `arg:"page_path,required"     desc:"Owning page the file is currently attached to."`
}

func (a *FileDetach) Doc() string { return "Remove a page/file attachment." }
func (a *FileDetach) Example() any {
	return &FileDetach{FilePath: "/files/checkout.js", PagePath: "/checkout"}
}
func (a *FileDetach) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	filePath := store.CanonicalFilePath(a.FilePath)
	n, err := store.UnlinkPageAssetCount(ctx.DB, a.PagePath, filePath)
	if err != nil {
		return tools.FailErr(err), nil
	}
	if n == 0 {
		return tools.FailCode(tools.CodeNotFound, "%s is not attached to %s (list_page_files shows what is)", filePath, a.PagePath), nil
	}
	return tools.Ok(map[string]any{
		"page_path": a.PagePath, "file_path": filePath, "detached": true,
	}), nil
}

// FileListPageAssets lists every file attached to a page.
type FileListPageAssets struct {
	PagePath string `arg:"page_path,required" desc:"Page whose attached files to list."`
}

func (a *FileListPageAssets) ActionName() string { return "list_page_files" }
func (a *FileListPageAssets) Doc() string        { return "List files attached to one page." }
func (a *FileListPageAssets) ReadOnly() bool     { return true }
func (a *FileListPageAssets) Example() any {
	return &FileListPageAssets{PagePath: "/checkout"}
}
func (a *FileListPageAssets) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	paths, err := store.PageAssets(ctx.DB, a.PagePath)
	if err != nil {
		return tools.FailErr(err), nil
	}
	return tools.Ok(map[string]any{
		"page_path": a.PagePath, "artifacts": paths, "count": len(paths),
	}), nil
}
