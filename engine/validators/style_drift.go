package validators

// The style-drift validator: surfaces pages that restyle themselves instead of using the
// project's shared stylesheet. Each page is written in a different task, so without a
// check the pages of one project slowly stop looking alike: one page re-derives the
// header offset in inline styles, another carries a few hundred lines of its own CSS,
// a third hardcodes the brand colours the tokens already name. Nothing breaks, which is
// why it survives, and the owner sees uneven pages.
//
// Only a project with two or more pages is judged (one page has nothing to drift from),
// and only page bodies: a shared stylesheet is where hex values and long rules belong.
// Deterministic error-severity so it drives a repair round; not in blockingCategories,
// so it never blocks a deploy. The thresholds are generous on purpose: a handful of
// inline styles for a computed width or a dynamic colour is ordinary.

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"openfabrica/store"
)

type styleDriftValidator struct{}

func (styleDriftValidator) Name() string { return "style_drift" }

const (
	// maxInlineStyles is how many style="" attributes one page may carry.
	maxInlineStyles = 10
	// maxPageStyleBytes is how much CSS one page may carry in its own <style> blocks.
	maxPageStyleBytes = 2500
	// maxPageHexColors is how many hex colours one page may hardcode in its inline and
	// <style> CSS.
	maxPageHexColors = 8
)

var (
	sdInlineRe = regexp.MustCompile(`(?is)\sstyle\s*=\s*("[^"]*"|'[^']*')`)
	sdBlockRe  = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>`)
	sdHexRe    = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
)

func (styleDriftValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	if pdb == nil {
		return nil
	}
	rows, err := pdb.Query(`SELECT path, COALESCE(body,'') FROM of_files
		WHERE kind = 'page' AND is_deleted = 0 AND status = 'published' ORDER BY path`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type page struct{ path, body string }
	var pages []page
	for rows.Next() {
		var p page
		if rows.Scan(&p.path, &p.body) == nil && strings.TrimSpace(p.body) != "" {
			pages = append(pages, p)
		}
	}
	if rows.Err() != nil || len(pages) < 2 {
		return nil
	}
	shared := sharedStylesheetPaths(pdb)

	var out []Issue
	for _, p := range pages {
		inline := sdInlineRe.FindAllStringSubmatch(p.body, -1)
		var css strings.Builder
		for _, m := range sdBlockRe.FindAllStringSubmatch(p.body, -1) {
			css.WriteString(m[1])
			css.WriteByte('\n')
		}
		hexes := len(sdHexRe.FindAllString(css.String(), -1))
		for _, m := range inline {
			hexes += len(sdHexRe.FindAllString(m[1], -1))
		}

		var problems []string
		if len(inline) > maxInlineStyles {
			problems = append(problems, fmt.Sprintf("%d inline style attributes (more than %d)", len(inline), maxInlineStyles))
		}
		if css.Len() > maxPageStyleBytes {
			problems = append(problems, fmt.Sprintf("%d bytes of its own <style> CSS (more than %d)", css.Len(), maxPageStyleBytes))
		}
		if hexes > maxPageHexColors {
			problems = append(problems, fmt.Sprintf("%d hardcoded hex colours (more than %d)", hexes, maxPageHexColors))
		}
		if len(problems) == 0 {
			continue
		}
		target := "the shared stylesheet"
		if len(shared) > 0 {
			target = shared[0]
		}
		out = append(out, Issue{
			Severity:      store.SeverityError,
			Category:      "style_drift",
			Page:          p.path,
			Deterministic: true,
			Signature:     "style_drift|" + p.path,
			Message: fmt.Sprintf("page %q styles itself instead of using the project's shared styles: %s. "+
				"Pages built in separate steps drift apart this way (different spacing, headings and colours from page to page). "+
				"Move the repeated rules into %s as named classes built on its design tokens (var(--...)), use those classes in this page, "+
				"and keep only values that are computed at runtime inline. The other pages must keep looking the same after the change.",
				p.path, strings.Join(problems, "; "), target),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	return out
}

// sharedStylesheetPaths lists the project's own stylesheets, largest first, so a finding
// can name the file the rules belong in.
func sharedStylesheetPaths(pdb *sql.DB) []string {
	rows, err := pdb.Query(`SELECT path, COALESCE(metadata,'') FROM of_files
		WHERE kind = 'file' AND is_deleted = 0 AND path LIKE '%.css' ORDER BY LENGTH(COALESCE(body,'')) DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path, meta string
		if rows.Scan(&path, &meta) == nil && !platformManaged(meta) {
			out = append(out, path)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

func init() {
	RegisterValidator(styleDriftValidator{})
}
