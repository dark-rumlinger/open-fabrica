package validators

// The static internal-link validator: catches a nav link or anchor whose target
// route was never built - it 404s at runtime, but nothing else flags it (the
// runtime probe only loads routes that exist; link_readiness covers orphaned
// JS/CSS, not dead hrefs). Static (of_files), resolving each literal internal
// link the way the page server does (exact route, then a parameterized segment
// match), so a `/posts/hello` link to `/posts/:slug` is never a false positive.

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"

	"openfabrica/internal/endpoints"
)

type internalLinkValidator struct{}

func (internalLinkValidator) Name() string { return "internal_links" }

// linkAttrRe pulls the literal value out of href / form action attributes. Only
// STATIC literals match (the runtime probe covers what JS builds). `data-route`
// is deliberately NOT matched: in an app-shaped SPA it is a client-side VIEW
// switch the server never owns, so resolving it against saved routes would
// false-flag a working in-page view. The \b stops `transaction=` matching `action`.
var linkAttrRe = regexp.MustCompile(`(?i)\b(?:href|action)\s*=\s*["']([^"']*)["']`)

func (internalLinkValidator) Validate(_ context.Context, pdb *sql.DB, _, _ string) []Issue {
	routes := savedPageRoutes(pdb)
	if routes == nil {
		// Query failed: resolve against nothing would flag every link, so self-
		// skip. A project with genuinely zero pages is a different failure the
		// other gates own.
		return nil
	}

	rows, err := pdb.Query("SELECT path, COALESCE(body,'') FROM of_files WHERE kind IN ('page','layout') AND is_deleted = 0")
	if err != nil {
		return nil
	}
	defer rows.Close()

	type deadLink struct{ file, route string }
	var found []deadLink
	seen := map[string]bool{}
	for rows.Next() {
		var file, body string
		if rows.Scan(&file, &body) != nil {
			continue
		}
		for _, m := range linkAttrRe.FindAllStringSubmatch(body, -1) {
			route, ok := normalizeInternalLink(m[1])
			if !ok || routeResolves(route, routes) {
				continue
			}
			key := file + "\x00" + route
			if seen[key] {
				continue
			}
			seen[key] = true
			found = append(found, deadLink{file: file, route: route})
		}
	}
	if rows.Err() != nil {
		return nil // partial read: self-skip rather than guess
	}

	if len(found) == 0 {
		return nil
	}
	// Stable order so the fix-pass sees a deterministic issue list.
	sort.Slice(found, func(i, j int) bool {
		if found[i].file != found[j].file {
			return found[i].file < found[j].file
		}
		return found[i].route < found[j].route
	})

	issues := make([]Issue, 0, len(found))
	for _, d := range found {
		issues = append(issues, Issue{
			Severity: "error",
			Category: "dead_link",
			// Route in Page so each (file, route) fingerprints distinctly:
			// Issue.Fingerprint is category+page+file and excludes the message,
			// so two dead links in one file must differ here or collapse to one.
			Page:          d.route,
			File:          d.file,
			Deterministic: true,
			Message: "`" + d.file + "` links to `" + d.route + "`, which resolves to no saved page route - the link 404s at runtime. " +
				"Either save a `kind=page` at `" + d.route + "` (build the screen behind the link), or fix/remove the link. " +
				"Valid internal destinations are exactly the saved page routes; a client-built link must carry the full route prefix, and a `#`-fragment is never a route (an in-page scroll, or a client-side view switch in a single-page app).",
		})
	}
	return issues
}

// savedPageRoutes returns the set of live page routes plus the always-served
// system paths (so a link to them is never flagged). Returns nil on query error
// so the validator self-skips rather than false-flagging every link.
func savedPageRoutes(pdb *sql.DB) map[string]bool {
	rows, err := pdb.Query("SELECT path FROM of_files WHERE kind = 'page' AND is_deleted = 0")
	if err != nil {
		return nil
	}
	defer rows.Close()
	routes := map[string]bool{
		"/offline": true, // enable_pwa's offline fallback page
		"/404":     true, // custom not-found page is never linked in nav but is a valid target
	}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && p != "" {
			routes[p] = true
		}
	}
	if rows.Err() != nil {
		return nil // a partial route set would false-flag, exactly like a failed query
	}
	// Redirect sources resolve too: the public page handler checks redirects
	// before the page table, so a link to a redirect_from is not dead. Tolerant -
	// skip on any error, since the table may be absent in a minimal context.
	if rrows, rerr := pdb.Query("SELECT redirect_from FROM of_redirects"); rerr == nil {
		for rrows.Next() {
			var p string
			if rrows.Scan(&p) == nil && p != "" {
				routes[p] = true
			}
		}
		_ = rrows.Err() // tolerated like rerr above: redirects are an enrichment
		rrows.Close()
	}
	return routes
}

// normalizeInternalLink reduces a raw attribute value to the route it targets,
// or returns ok=false when it is not an internal page route to check (external
// URL, fragment, non-http scheme, a platform-served path such as /api or /feed, static asset, literal
// :param template, non-root-absolute, or empty).
func normalizeInternalLink(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "#" {
		return "", false
	}
	// Routing keys on the path only - drop query and fragment.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "", false
	}
	lower := strings.ToLower(s)
	for _, pfx := range []string{"http://", "https://", "//", "mailto:", "tel:", "javascript:", "data:", "blob:"} {
		if strings.HasPrefix(lower, pfx) {
			return "", false
		}
	}
	// Internal routes are root-absolute by contract; a bare relative ("about")
	// or "./x" is left alone rather than guessed at.
	if !strings.HasPrefix(s, "/") {
		return "", false
	}
	// Platform-served namespaces (/api, /files, /feed, /webhooks, /payments, /__of) are
	// not saved pages: the contracts tell the builder to link /feed/<path> from the head.
	if endpoints.IsPlatformRoute(s) {
		return "", false
	}
	// An author-written :param template is not a navigable link.
	if strings.Contains(s, ":") {
		return "", false
	}
	// Page routes are clean and extension-less; a final segment with a dot is a
	// static asset reference (favicon.svg, og.png, /manifest.json), not a page.
	last := s
	if i := strings.LastIndex(s, "/"); i >= 0 {
		last = s[i+1:]
	}
	if strings.Contains(last, ".") {
		return "", false
	}
	// Normalize a trailing slash (except root) so "/about/" matches "/about".
	if len(s) > 1 {
		if t := strings.TrimRight(s, "/"); t != "" {
			s = t
		}
	}
	return s, true
}

// routeResolves reports whether route is served: an exact saved route, or a
// parameterized template match. Mirrors server/public.findParamPage: equal
// segment count, every template segment is a ':param' or an exact literal.
func routeResolves(route string, routes map[string]bool) bool {
	if routes[route] {
		return true
	}
	reqParts := strings.Split(route, "/")
	for tmpl := range routes {
		if !strings.Contains(tmpl, ":") {
			continue
		}
		tmplParts := strings.Split(tmpl, "/")
		if len(tmplParts) != len(reqParts) {
			continue
		}
		match := true
		for i, cp := range tmplParts {
			if strings.HasPrefix(cp, ":") {
				continue
			}
			if cp != reqParts[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func init() {
	RegisterValidator(internalLinkValidator{})
}
