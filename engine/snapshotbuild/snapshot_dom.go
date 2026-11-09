package snapshotbuild

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"
)

// DOMContract is the static element-id surface of one page: the ids its markup
// declares (body + shared layout + included components), the hooks a page-scope
// script binds to. Surfaced so a module wires to the DOM the page ACTUALLY
// provides instead of inventing a container that throws at runtime, often behind
// an interaction the page-LOAD probe never triggers. The HTML analog of
// ModuleContract and StyleContract; only the id hooks are carried.
type DOMContract struct {
	Path      string
	IDs       []string
	Truncated bool
}

// domIDRe matches an element id attribute in HTML markup: id="foo" / id='foo'.
var domIDRe = regexp.MustCompile(`(?i)\bid\s*=\s*["']([A-Za-z_][\w:.-]*)["']`)

// includeRe matches a {{include:name}} component reference in a page/layout body.
var includeRe = regexp.MustCompile(`\{\{\s*include\s*:\s*([A-Za-z0-9_-]+)\s*\}\}`)

// maxDOMIDs caps a page's rendered id list so a huge page can't blow the snapshot;
// the overflow is flagged so the model knows to read the page for the rest.
const maxDOMIDs = 80

// extractElementIDs returns the unique element ids declared in an HTML body, sorted.
func extractElementIDs(body string) []string {
	seen := map[string]bool{}
	for _, m := range domIDRe.FindAllStringSubmatch(body, -1) {
		seen[m[1]] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// loadHTMLBodies reads the path->body map for one artifact kind (page / layout /
// component). Best-effort, like every other snapshot read.
func loadHTMLBodies(ctx context.Context, db *sql.DB, kind string) map[string]string {
	out := map[string]string{}
	rows, err := db.QueryContext(ctx,
		`SELECT path, COALESCE(body,'') FROM of_files WHERE kind = ? AND COALESCE(is_deleted,0) = 0`, kind)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p, b string
		if rows.Scan(&p, &b) == nil && p != "" {
			out[p] = b
		}
	}
	_ = rows.Err()
	return out
}

// hasPageScopeJS reports whether the project carries any page-scope JS module: the
// signal that something will bind to a page's DOM, whether or not an entry has been
// attached yet.
func hasPageScopeJS(files []FileRef) bool {
	for _, f := range files {
		if f.Scope == "page" && strings.HasSuffix(f.Path, ".js") {
			return true
		}
	}
	return false
}

// buildDOMContracts collects, per JS-carrying page, the element ids its static markup
// declares (the page body, shared layout(s), and any {{include}}d components), the HTML->JS
// binding contract. Scoped to pages with a page-scope .js asset (jsPages): a page with no
// script has no DOM to wire. See DOMContract.
func buildDOMContracts(ctx context.Context, db *sql.DB, jsPages map[string]bool) []DOMContract {
	if len(jsPages) == 0 {
		return nil
	}
	pages := loadHTMLBodies(ctx, db, "page")
	// Ids the shared layout(s) contribute to every rendered page.
	layoutIDs := map[string]bool{}
	for _, b := range loadHTMLBodies(ctx, db, "layout") {
		for _, id := range extractElementIDs(b) {
			layoutIDs[id] = true
		}
	}
	compIDs := map[string][]string{}
	for name, b := range loadHTMLBodies(ctx, db, "component") {
		compIDs[name] = extractElementIDs(b)
	}

	paths := make([]string, 0, len(pages))
	for p := range pages {
		if jsPages[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var out []DOMContract
	for _, p := range paths {
		body := pages[p]
		set := map[string]bool{}
		for id := range layoutIDs {
			set[id] = true
		}
		for _, id := range extractElementIDs(body) {
			set[id] = true
		}
		for _, m := range includeRe.FindAllStringSubmatch(body, -1) {
			for _, id := range compIDs[m[1]] {
				set[id] = true
			}
		}
		if len(set) == 0 {
			continue
		}
		ids := make([]string, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		trunc := false
		if len(ids) > maxDOMIDs {
			ids, trunc = ids[:maxDOMIDs], true
		}
		out = append(out, DOMContract{Path: p, IDs: ids, Truncated: trunc})
	}
	return out
}
