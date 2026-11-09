package store

import (
	"sort"
	"strings"

	"openfabrica/analysis"
	"openfabrica/internal/dbh"
)

// isPageJS reports whether a page artifact path is a JS/ESM module the import
// graph can traverse. CSS is excluded: a browser ES module cannot pull a
// stylesheet in, so CSS must always be linked directly.
func isPageJS(p string) bool {
	lp := strings.ToLower(p)
	return strings.HasSuffix(lp, ".js") || strings.HasSuffix(lp, ".mjs")
}

// UnlinkedPageArtifacts returns the paths of every page-scoped JS/CSS artifact
// neither directly linked via of_page_assets NOR reachable through the ES-module
// import graph from a linked JS entry - dead weight or a missing-link bug. Shared
// by the diagnostics tool and the post-build holistic validator (one source of
// truth). CSS is never reached via the graph and so must always be linked directly.
func UnlinkedPageArtifacts(db dbh.Handle) []string {
	return UnlinkedPageArtifactsClassified(db).Orphans
}

// UnlinkedPageArtifactsResult classifies the orphaned page artifacts by how
// confidently they can be called dead. Uncertain is true when a module in the
// REACHABLE CLOSURE contains an OPAQUE dynamic import whose target the static
// graph cannot resolve - such an import can load any orphan at runtime, so
// calling the orphans dead is speculation. CSS is never affected (an ES module
// cannot import a stylesheet).
type UnlinkedPageArtifactsResult struct {
	Orphans   []string // page artifacts neither linked nor reachable via the static graph
	Uncertain bool     // an opaque dynamic import in the reachable closure could be wiring them
}

// UnlinkedPageArtifactsClassified returns the orphan set and whether it is
// uncertain (an opaque dynamic import in the reachable closure). The
// link-readiness validator uses Uncertain to downgrade orphans to a non-looping warning.
// FlowScriptPath is the user-flow script (of_test.js). manage_testing stores it under
// its own kind; a stray kind=file copy of it (older builds let manage_files make one)
// is test data, never a page module, so it is never reported as unlinked - a finding
// on it sent a fix-pass to attach the test script to the live page.
const FlowScriptPath = "of_test.js"

func UnlinkedPageArtifactsClassified(db dbh.Handle) UnlinkedPageArtifactsResult {
	if db == nil {
		return UnlinkedPageArtifactsResult{}
	}
	rows, err := db.Query(`
		SELECT path, COALESCE(body, '') FROM of_files
		WHERE kind = 'file' AND scope = 'page' AND is_deleted = 0 AND path != ?
		  AND (path LIKE '%.js' OR path LIKE '%.mjs' OR path LIKE '%.css')`, FlowScriptPath)
	if err != nil {
		return UnlinkedPageArtifactsResult{}
	}
	defer rows.Close()

	type pageArt struct {
		path string
		body string
	}
	var arts []pageArt
	for rows.Next() {
		var p, body string
		if rows.Scan(&p, &body) == nil && p != "" {
			arts = append(arts, pageArt{path: p, body: body})
		}
	}
	if rows.Err() != nil {
		return UnlinkedPageArtifactsResult{} // incomplete read; report nothing, same as the query error
	}
	if len(arts) == 0 {
		return UnlinkedPageArtifactsResult{}
	}

	// Directly linked set.
	linked := make(map[string]bool)
	if lrows, lerr := db.Query(`SELECT file_path FROM of_page_assets`); lerr == nil {
		for lrows.Next() {
			var p string
			if lrows.Scan(&p) == nil {
				linked[p] = true
			}
		}
		_ = lrows.Err() // tolerated like the query error above
		lrows.Close()
	}

	// Build the ES-module import graph over the page-JS artifacts via the shared
	// builder, so reachability resolves specifiers exactly as the runtime server
	// and the readiness validator do. CSS never participates (a module cannot
	// import a stylesheet), so the graph is built over the JS subset only.
	jsBodies := make(map[string]string)
	for _, a := range arts {
		if isPageJS(a.path) {
			jsBodies[a.path] = a.body
		}
	}
	graph := analysis.BuildModuleGraph(jsBodies)

	// Reachable = transitive closure of imports from every linked JS entry.
	reachable := make(map[string]bool)
	var visit func(p string)
	visit = func(p string) {
		for _, dep := range graph.Edges[p] {
			if !reachable[dep] {
				reachable[dep] = true
				visit(dep)
			}
		}
	}
	for _, a := range arts {
		if linked[a.path] && isPageJS(a.path) {
			visit(a.path)
		}
	}

	var out []string
	for _, a := range arts {
		if linked[a.path] || reachable[a.path] {
			continue
		}
		out = append(out, a.path)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return UnlinkedPageArtifactsResult{}
	}

	// An opaque dynamic import anywhere in the reachable closure can load any
	// orphan at runtime, so the static graph cannot prove the orphans dead.
	// Comments are stripped so a commented-out import() never triggers a false
	// downgrade; an orphan's own opaque import is not in the closure and is ignored.
	uncertain := false
	for p, body := range jsBodies {
		if !(linked[p] || reachable[p]) {
			continue
		}
		for _, d := range analysis.ExtractDynamicImportArgs(analysis.StripJSComments(body)) {
			if !d.Literal {
				uncertain = true
				break
			}
		}
		if uncertain {
			break
		}
	}
	return UnlinkedPageArtifactsResult{Orphans: out, Uncertain: uncertain}
}

// PageForAsset returns one page path that links the given file via
// of_page_assets, or ("", false) when unattached. A page-scope script only runs
// as intended on the page that loads it, so a probe navigates here instead of
// loading it bare. Deterministic (lowest page_path) when attached to several pages.
func PageForAsset(db dbh.Handle, filePath string) (string, bool) {
	if db == nil || filePath == "" {
		return "", false
	}
	var page string
	err := db.QueryRow(`
		SELECT page_path FROM of_page_assets
		WHERE file_path = ?
		ORDER BY page_path
		LIMIT 1`, filePath).Scan(&page)
	if err != nil || page == "" {
		return "", false
	}
	return page, true
}

// PagesAttaching returns every page that links filePath via of_page_assets, sorted.
func PagesAttaching(db dbh.Handle, filePath string) ([]string, error) {
	rows, err := db.Query(`SELECT page_path FROM of_page_assets WHERE file_path = ? ORDER BY page_path`, filePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// LinkPageAsset records "include filePath when serving pagePath" in
// of_page_assets. Idempotent, re-linking the same pair updates position.
func LinkPageAsset(db dbh.Handle, pagePath, filePath string, position int) error {
	_, err := db.Exec(`INSERT INTO of_page_assets (page_path, file_path, position)
		VALUES (?, ?, ?)
		ON CONFLICT(page_path, file_path) DO UPDATE SET position = excluded.position`,
		pagePath, filePath, position)
	return err
}

// UnlinkPageAssetCount removes the link and reports how many rows it removed (0 when
// the file was not attached to that page).
func UnlinkPageAssetCount(db dbh.Handle, pagePath, filePath string) (int64, error) {
	res, err := db.Exec(`DELETE FROM of_page_assets WHERE page_path = ? AND file_path = ?`, pagePath, filePath)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PageAssets returns the ordered list of artifact paths linked to a page.
func PageAssets(db dbh.Handle, pagePath string) ([]string, error) {
	rows, err := db.Query(`SELECT file_path FROM of_page_assets WHERE page_path = ? ORDER BY position, file_path`, pagePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FileScope returns a kind=file artifact's scope ("global" | "page"), or "" when it
// does not exist. Callers about to change how an artifact ships must read it first:
// flipping a global file to page scope takes it off every page but one, and no gate
// downstream reports the pages it just unstyled.
func FileScope(db dbh.Handle, filePath string) string {
	var s string
	if err := db.QueryRow(
		`SELECT COALESCE(scope,'') FROM of_files WHERE kind = 'file' AND path = ? AND is_deleted = 0`,
		filePath).Scan(&s); err != nil {
		return ""
	}
	return s
}

// PromotePageAssetScope flips an artifact's scope to "page", resolving a
// global-then-attached contradiction toward the attach. ONLY safe when no other
// page exists (linkFileToPage, the one caller, refuses the demotion otherwise).
func PromotePageAssetScope(db dbh.Handle, filePath string) (bool, error) {
	res, err := db.Exec(
		`UPDATE of_files SET scope = 'page'
		 WHERE kind = 'file' AND path = ? AND is_deleted = 0 AND scope IS NOT 'page'`,
		filePath)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AnyPageExists reports whether the project has a page at all. A page-scope file
// in a project with none has nothing that could ever load it, which is a different
// gap from a file whose page simply has not been linked yet.
func AnyPageExists(db dbh.Handle) bool {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND COALESCE(is_deleted,0) = 0`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// OtherPagePaths returns the project's page routes excluding pagePath - the pages that
// would LOSE a global file if it were demoted to page scope, the whole harm of demoting.
func OtherPagePaths(db dbh.Handle, pagePath string) []string {
	rows, err := db.Query(
		`SELECT path FROM of_files WHERE kind = 'page' AND COALESCE(is_deleted,0) = 0 AND path != ? ORDER BY path`,
		pagePath)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && p != "" {
			out = append(out, p)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}
