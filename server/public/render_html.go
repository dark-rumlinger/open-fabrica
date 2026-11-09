package public

import (
	"database/sql"
	"log/slog"
	"regexp"
	"strings"
)

// seoOverride is the subset of of_seo_meta the renderer consumes. Fields are
// empty when the column is empty so the caller can use a single non-empty
// check to decide whether to apply the override.
type seoOverride struct {
	Title          string
	Description    string
	OGTitle        string
	OGDescription  string
	OGImage        string
	CanonicalURL   string
	StructuredData string
	TwitterCard    string
	Locale         string
}

// loadSEOMeta returns per-page SEO overrides, or nil when no row exists for
// pagePath. Missing rows are silent (typical case); we only log when the
// query itself fails so a misconfigured DB shows up in logs.
func loadSEOMeta(projectDB *sql.DB, pagePath string) *seoOverride {
	var s seoOverride
	err := projectDB.QueryRow(
		`SELECT title, description, COALESCE(og_title, ''), COALESCE(og_description, ''), og_image, canonical_url, structured_data, twitter_card, locale
		 FROM of_seo_meta WHERE page_path = ?`, pagePath,
	).Scan(&s.Title, &s.Description, &s.OGTitle, &s.OGDescription, &s.OGImage, &s.CanonicalURL, &s.StructuredData, &s.TwitterCard, &s.Locale)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		slog.Warn("loadSEOMeta query failed", "path", pagePath, "error", err)
		return nil
	}
	return &s
}

// isRTLLocale reports whether a BCP-47 tag's primary subtag is a known
// right-to-left script. Covers Arabic, Hebrew, Persian, Urdu, Yiddish.
func isRTLLocale(tag string) bool {
	primary := tag
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		primary = tag[:i]
	}
	switch strings.ToLower(primary) {
	case "ar", "he", "fa", "ur", "yi":
		return true
	}
	return false
}

// insertBeforeTag inserts payload immediately before the first case-insensitive
// occurrence of tag in doc. If the tag is missing, appends payload to doc with
// a warning-free best-effort fallback (so pages still render).
func insertBeforeTag(doc, tag, payload string) string {
	if payload == "" {
		return doc
	}
	lower := strings.ToLower(doc)
	idx := strings.Index(lower, strings.ToLower(tag))
	if idx < 0 {
		// Tag missing: append payload so it isn't silently dropped. A layout
		// without </head> or </body> is malformed but still renders.
		return doc + "\n" + payload
	}
	return doc[:idx] + payload + doc[idx:]
}

// stripSharedAssetRefs removes <link> and <script src> tags referencing /files/
// from page content. These are auto-injected by the server, so duplicates in
// page content would cause double-loading.
func stripSharedAssetRefs(content string, projectDB *sql.DB) string {
	// Collect known asset filenames for matching.
	assetNames := map[string]bool{}
	rows, err := projectDB.Query("SELECT path FROM of_files WHERE kind = 'file' AND is_deleted = 0 AND (path LIKE '%.css' OR path LIKE '%.js')")
	if err != nil {
		return content
	}
	defer rows.Close()
	for rows.Next() {
		var fn string
		if rows.Scan(&fn) == nil {
			assetNames[strings.ToLower(fn)] = true
		}
	}
	// A failed iteration is handled like a failed query: leave content as is.
	if rows.Err() != nil {
		return content
	}
	if len(assetNames) == 0 {
		return content
	}

	// Remove <link rel="stylesheet" href="/files/..."> for known assets.
	content = cssLinkRe.ReplaceAllStringFunc(content, func(match string) string {
		lower := strings.ToLower(match)
		for name := range assetNames {
			if strings.Contains(lower, "/files/"+name) {
				return ""
			}
		}
		return match
	})

	// Remove <script src="/files/...">...</script> for known assets.
	// Only check the opening <script> tag for src=, not the script body.
	content = scriptBlockRe.ReplaceAllStringFunc(content, func(match string) string {
		if !isExternalScript(match) {
			return match // inline script, keep it
		}
		lower := strings.ToLower(match)
		for name := range assetNames {
			if strings.Contains(lower, "/files/"+name) {
				return ""
			}
		}
		return match
	})

	// Remove self-closing <script src="/files/..." /> for known assets.
	content = scriptSelfCloseRe.ReplaceAllStringFunc(content, func(match string) string {
		lower := strings.ToLower(match)
		for name := range assetNames {
			if strings.Contains(lower, "/files/"+name) {
				return ""
			}
		}
		return match
	})

	return content
}

// Asset tag extraction, hoists CSS to <head> and JS to end of <body>.

// Regex patterns for extracting asset tags from page content.
var (
	// Matches <link ... rel="stylesheet" ... > or <link ... rel='stylesheet' ... >
	cssLinkRe = regexp.MustCompile(`(?i)<link[^>]*rel=["']stylesheet["'][^>]*/?>`)
	// Matches <style> ... </style> blocks (including attributes)
	styleBlockRe = regexp.MustCompile(`(?isU)<style[\s>].*</style>`)
	// Matches <script ...> ... </script> blocks (inline or src-based)
	scriptBlockRe = regexp.MustCompile(`(?isU)<script[\s>].*</script>`)
	// Matches self-closing <script ... /> (rare but valid)
	scriptSelfCloseRe = regexp.MustCompile(`(?i)<script[^>]*/\s*>`)
	// Matches the opening <script ...> tag (up to the first '>').
	scriptOpenTagRe = regexp.MustCompile(`(?i)<script[^>]*>`)
	// Matches a src attribute inside a tag.
	scriptSrcRe = regexp.MustCompile(`(?i)\bsrc\s*=`)
	// Attribute extractors for SPA asset URL extraction.
	hrefAttrRe = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
	srcAttrRe  = regexp.MustCompile(`(?i)src\s*=\s*["']([^"']+)["']`)
)

// extractHref returns the href attribute value from an HTML tag, or "".
func extractHref(tag string) string {
	m := hrefAttrRe.FindStringSubmatch(tag)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// extractSrc returns the src attribute value from an HTML tag, or "".
func extractSrc(tag string) string {
	m := srcAttrRe.FindStringSubmatch(tag)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

// isExternalScript reports whether a <script> block has a src attribute on its
// opening tag. Checks only the opening tag so 'src=' inside JS (e.g. a template
// literal) is not a false positive.
func isExternalScript(block string) bool {
	openTag := scriptOpenTagRe.FindString(block)
	if openTag == "" {
		return false
	}
	return scriptSrcRe.MatchString(openTag)
}

// extractScriptSrc extracts the src URL from a <script> block's opening tag only.
func extractScriptSrc(block string) string {
	openTag := scriptOpenTagRe.FindString(block)
	if openTag == "" {
		return ""
	}
	return extractSrc(openTag)
}

// dedupStrings returns a slice with duplicate strings removed, preserving order.
func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// componentIncludeRe matches {{include:component_name}} placeholders.
var componentIncludeRe = regexp.MustCompile(`\{\{include:([a-zA-Z0-9_-]+)\}\}`)

// resolveComponents replaces {{include:name}} placeholders in page content
// with the corresponding component HTML from of_files (kind=component).
// Limits to 20 replacements per page to prevent infinite recursion.
func resolveComponents(projectDB *sql.DB, content string) string {
	if !strings.Contains(content, "{{include:") {
		return content
	}

	matches := componentIncludeRe.FindAllStringSubmatch(content, 20)
	if len(matches) == 0 {
		return content
	}

	// Deduplicate component names and batch-load.
	nameSet := make(map[string]bool, len(matches))
	for _, m := range matches {
		nameSet[m[1]] = true
	}

	cache := make(map[string]string, len(nameSet))
	for name := range nameSet {
		var componentContent string
		if err := projectDB.QueryRow("SELECT body FROM of_files WHERE kind = 'component' AND path = ? AND is_deleted = 0", name).Scan(&componentContent); err == nil {
			cache[name] = componentContent
		}
	}

	return componentIncludeRe.ReplaceAllStringFunc(content, func(match string) string {
		name := match[10 : len(match)-2] // strip "{{include:" and "}}"
		if html, ok := cache[name]; ok {
			return html
		}
		return match // leave unresolved if component not found
	})
}

// extractAssetTags separates CSS and JS tags from page content so they can be
// placed in the correct locations: CSS in <head>, JS at end of <body>.
func extractAssetTags(content string) (headTags, bodyEndTags, cleanContent string) {
	var head []string
	var bodyEnd []string

	// Extract <link rel="stylesheet"> tags -> head
	content = cssLinkRe.ReplaceAllStringFunc(content, func(match string) string {
		head = append(head, match)
		return ""
	})

	// Extract <style> blocks -> head
	content = styleBlockRe.ReplaceAllStringFunc(content, func(match string) string {
		head = append(head, match)
		return ""
	})

	// Extract <script> blocks -> body end
	content = scriptBlockRe.ReplaceAllStringFunc(content, func(match string) string {
		bodyEnd = append(bodyEnd, match)
		return ""
	})

	// Extract self-closing <script /> -> body end
	content = scriptSelfCloseRe.ReplaceAllStringFunc(content, func(match string) string {
		bodyEnd = append(bodyEnd, match)
		return ""
	})

	headTags = strings.Join(head, "\n")
	bodyEndTags = strings.Join(bodyEnd, "\n")
	cleanContent = content
	return
}

// stripForSPA prepares page content for the SPA JSON endpoint (/api/page).
// Strips CSS links and external scripts but keeps <style> blocks and inline
// scripts. Returns the extracted CSS and JS URLs for the SPA router to load.
func stripForSPA(content string) (string, []string, []string) {
	var cssURLs, jsURLs []string

	// Extract CSS link hrefs, then remove the tags.
	for _, match := range cssLinkRe.FindAllString(content, -1) {
		if href := extractHref(match); href != "" {
			cssURLs = append(cssURLs, href)
		}
	}
	content = cssLinkRe.ReplaceAllString(content, "")
	// Keep <style> blocks, they work in innerHTML and may contain page-specific styles.

	// Extract external script srcs, then remove the tags; keep inline scripts.
	// Only the opening <script> tag is checked for src=, not the body (JS often
	// contains 'src=' in template literals).
	content = scriptBlockRe.ReplaceAllStringFunc(content, func(match string) string {
		if isExternalScript(match) {
			if src := extractScriptSrc(match); src != "" {
				jsURLs = append(jsURLs, src)
			}
			return "" // external script, strip it
		}
		return match // inline script, keep it
	})

	// Remove self-closing <script src="..." /> tags, extract src.
	content = scriptSelfCloseRe.ReplaceAllStringFunc(content, func(match string) string {
		if src := extractSrc(match); src != "" {
			jsURLs = append(jsURLs, src)
		}
		return ""
	})

	return strings.TrimSpace(content), cssURLs, jsURLs
}

// Document shell stripping, removes structural HTML tags from LLM content.

var (
	doctypeRe   = regexp.MustCompile(`(?i)<!DOCTYPE[^>]*>`)
	htmlOpenRe  = regexp.MustCompile(`(?i)<html[^>]*>`)
	htmlCloseRe = regexp.MustCompile(`(?i)</html\s*>`)
	headBlockRe = regexp.MustCompile(`(?isU)<head[\s>].*</head>`)
	bodyOpenRe  = regexp.MustCompile(`(?i)<body[^>]*>`)
	bodyCloseRe = regexp.MustCompile(`(?i)</body\s*>`)

	// Singleton head tags the server always emits in headBlob. When a layout
	// template authors its own copies they must be removed before injection, or
	// the document ends up with two <title>/charset/viewport tags.
	headTitleRe    = regexp.MustCompile(`(?is)<title[^>]*>.*?</title>`)
	headCharsetRe  = regexp.MustCompile(`(?i)<meta[^>]+charset[^>]*>`)
	headViewportRe = regexp.MustCompile(`(?i)<meta[^>]+name=["']viewport["'][^>]*>`)
)

// interpolateRouteParams substitutes {name} placeholders in a title with the
// matched route params. Unmatched placeholders and titles with none are left
// unchanged, so this is a no-op for static pages.
func interpolateRouteParams(title string, routeParams map[string]string) string {
	if len(routeParams) == 0 || !strings.Contains(title, "{") {
		return title
	}
	for name, value := range routeParams {
		title = strings.ReplaceAll(title, "{"+name+"}", value)
	}
	return title
}

// stripOwnedHeadTags removes the singleton tags the server owns (title, charset,
// viewport) from a layout template's <head>, so headBlob is their sole source.
// Scoped to the <head> block to avoid touching an SVG <title> or body meta.
func stripOwnedHeadTags(template string) string {
	return headBlockRe.ReplaceAllStringFunc(template, func(head string) string {
		head = headTitleRe.ReplaceAllString(head, "")
		head = headCharsetRe.ReplaceAllString(head, "")
		head = headViewportRe.ReplaceAllString(head, "")
		return head
	})
}

// stripDocumentShell removes structural HTML tags (DOCTYPE, html, head, body)
// from page content, since renderDocument supplies its own shell. CSS/JS inside
// <head> are rescued so extractAssetTags can hoist them later.
func stripDocumentShell(content string) string {
	lower := strings.ToLower(content)
	if !strings.Contains(lower, "<!doctype") && !strings.Contains(lower, "<html") {
		return content
	}

	// Rescue <style>, <link rel="stylesheet">, and <script> from inside <head>
	// before we remove the entire <head> block.
	var rescued []string
	for _, headMatch := range headBlockRe.FindAllString(content, -1) {
		rescued = append(rescued, cssLinkRe.FindAllString(headMatch, -1)...)
		rescued = append(rescued, styleBlockRe.FindAllString(headMatch, -1)...)
		rescued = append(rescued, scriptBlockRe.FindAllString(headMatch, -1)...)
	}

	content = doctypeRe.ReplaceAllString(content, "")
	content = headBlockRe.ReplaceAllString(content, "")
	content = htmlOpenRe.ReplaceAllString(content, "")
	content = htmlCloseRe.ReplaceAllString(content, "")
	content = bodyOpenRe.ReplaceAllString(content, "")
	content = bodyCloseRe.ReplaceAllString(content, "")

	if len(rescued) > 0 {
		content = strings.Join(rescued, "\n") + "\n" + content
	}

	return strings.TrimSpace(content)
}
