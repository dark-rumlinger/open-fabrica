package snapshotbuild

import (
	"context"
	"database/sql"
	"sort"

	"openfabrica/analysis"
)

// StyleContract is the design-system surface of one global stylesheet: the custom
// properties (tokens), class names, and @keyframes names it defines. Surfaced so a
// page built after a context rollover (stylesheet body no longer in the transcript) reuses
// the exact tokens and classes instead of re-inventing them. The CSS analog of
// ModuleContract. VarValues carries each token's declared value on the shared design
// system only, so pages compose from real values; class and keyframe bodies never are.
type StyleContract struct {
	Path      string
	Vars      []string          // custom properties, leading -- kept: "--color-bg"
	VarValues map[string]string // token values, global stylesheets only; nil for page one-offs
	Classes   []string          // class selector names, leading . dropped: "btn", "card"
	Keyframes []string          // @keyframes / animation names
	Truncated bool              // a per-list cap or the framework-dump guard fired
}

// PageStyleContract is the names-only style surface of ONE page's page-scope
// stylesheet(s). The CSS twin of DOMContract, rendered under its own header so
// it never reads as shared design vocabulary: global CSS stays in Styles (REUSE
// on every page); a page's own classes are one-offs another page must NOT copy,
// which is why buildStyleContracts filters to scope='global'.
type PageStyleContract struct {
	Page   string          // the page route these stylesheets are linked to
	Sheets []StyleContract // its page-scope stylesheets, by path
}

// buildStyleContracts parses the global stylesheet(s) into a names-only design-system
// contract (the custom properties and class names a page reuses). Global-scope CSS only,
// the shared design system by convention; per-page CSS is excluded to avoid one-offs other
// pages should not copy. A file with no vars/classes/keyframes is skipped (its zero contract
// is cached so the body is not re-parsed). Sorted by path.
func buildStyleContracts(ctx context.Context, db *sql.DB, cache *ModuleParseCache) []StyleContract {
	rows, err := db.QueryContext(ctx, `
		SELECT path, COALESCE(body,'') FROM of_files
		WHERE kind = 'file' AND COALESCE(is_deleted,0) = 0
		  AND scope = 'global' AND path LIKE '%.css'
		ORDER BY path`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []StyleContract
	for rows.Next() {
		var p, body string
		if rows.Scan(&p, &body) != nil || p == "" {
			continue
		}
		if sc, ok := cache.getStyle(p, body); ok {
			// A cached empty (no surface) is recorded as a zero contract; honour
			// it so we don't re-parse the same body next turn.
			if len(sc.Vars) == 0 && len(sc.Classes) == 0 && len(sc.Keyframes) == 0 {
				continue
			}
			out = append(out, sc)
			continue
		}
		ss := analysis.ExtractStyleSurface(body)
		sc := StyleContract{Path: p, Vars: ss.Vars, VarValues: ss.VarValues, Classes: ss.Classes, Keyframes: ss.Keyframes, Truncated: ss.Truncated}
		cache.putStyle(p, body, sc)
		if len(sc.Vars) == 0 && len(sc.Classes) == 0 && len(sc.Keyframes) == 0 {
			continue
		}
		out = append(out, sc)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// buildPageStyleContracts parses each page's page-scope stylesheet(s) into the
// same names-only surface buildStyleContracts produces, keyed to the page that
// links them. Kept separate from the shared design system (a page one-off is not
// shared vocabulary). Gated and capped like the DOM contract, so growth follows
// pages-that-have-CSS, not page count.
func buildPageStyleContracts(ctx context.Context, db *sql.DB, cache *ModuleParseCache, pages []PageRef) []PageStyleContract {
	if len(pages) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT path, COALESCE(body,'') FROM of_files
		WHERE kind = 'file' AND COALESCE(is_deleted,0) = 0
		  AND scope = 'page' AND path LIKE '%.css'
		ORDER BY path`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	bodies := map[string]string{}
	for rows.Next() {
		var p, body string
		if rows.Scan(&p, &body) != nil || p == "" {
			continue
		}
		bodies[p] = body
	}
	if rows.Err() != nil || len(bodies) == 0 {
		return nil
	}

	ordered := make([]PageRef, len(pages))
	copy(ordered, pages)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })

	var out []PageStyleContract
	for _, p := range ordered {
		var sheets []StyleContract
		for _, a := range p.Assets {
			body, ok := bodies[a]
			if !ok {
				continue
			}
			sc, cached := cache.getStyle(a, body)
			if !cached {
				ss := analysis.ExtractStyleSurface(body)
				sc = StyleContract{Path: a, Vars: ss.Vars, Classes: ss.Classes, Keyframes: ss.Keyframes, Truncated: ss.Truncated}
				cache.putStyle(a, body, sc)
			}
			if len(sc.Vars) == 0 && len(sc.Classes) == 0 && len(sc.Keyframes) == 0 {
				continue // no surface to state
			}
			sheets = append(sheets, sc)
		}
		if len(sheets) == 0 {
			continue
		}
		out = append(out, PageStyleContract{Page: p.Path, Sheets: sheets})
	}
	return out
}
