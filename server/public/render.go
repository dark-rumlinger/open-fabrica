package public

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"strings"

	"openfabrica/analysis"
	"openfabrica/db/models"
)

// stampInlineScriptNonce adds nonce="<nonce>" to every inline <script> (no src=,
// no existing nonce) in doc. No-op unless a strict nonce-based CSP is active
// (middleware.NonceFromContext). External src= scripts still load via 'self'; the
// renderer's own inline scripts need the nonce once 'unsafe-inline' is dropped.
func stampInlineScriptNonce(doc, nonce string) string {
	if nonce == "" {
		return doc
	}
	const tag = "<script"
	lower := strings.ToLower(doc)
	var b strings.Builder
	b.Grow(len(doc) + 64)
	i := 0
	for {
		rel := strings.Index(lower[i:], tag)
		if rel < 0 {
			b.WriteString(doc[i:])
			break
		}
		open := i + rel
		gt := strings.IndexByte(doc[open:], '>')
		if gt < 0 {
			b.WriteString(doc[i:]) // malformed/truncated tag: emit verbatim
			break
		}
		gt += open
		attrs := strings.ToLower(doc[open+len(tag) : gt])
		b.WriteString(doc[i : open+len(tag)]) // up to and including "<script"
		if !strings.Contains(attrs, "src=") && !strings.Contains(attrs, "nonce=") {
			b.WriteString(` nonce="` + nonce + `"`)
		}
		b.WriteString(doc[open+len(tag) : gt+1]) // the attributes and closing '>'
		i = gt + 1
	}
	return b.String()
}

// currentPlanJSONQuery returns the project's current plan_json: the most recent
// of_runs row carrying one (written at PLAN). Mirrors the query in
// tools/project/manage_libraries.go and engine.latestCompletedPlan.
const currentPlanJSONQuery = `SELECT plan_json FROM of_runs
	WHERE plan_json IS NOT NULL AND plan_json != ''
	ORDER BY started_at DESC, id DESC LIMIT 1`

type layoutData struct {
	Template string
}

// loadLayout loads a layout by name, falling back to "default" then to the
// most-recently-updated layout when the name is missing, so a project that saved
// one layout under any name still gets it applied without every page setting
// metadata.layout. Returns nil only when no layouts exist (renderer then
// synthesizes a minimal shell).
func loadLayout(projectDB *sql.DB, name string) *layoutData {
	if body := readLayoutBody(projectDB, name); body != "" {
		return &layoutData{Template: body}
	}
	if name != "default" {
		if body := readLayoutBody(projectDB, "default"); body != "" {
			return &layoutData{Template: body}
		}
	}
	var fallbackBody sql.NullString
	err := projectDB.QueryRow(
		`SELECT body FROM of_files
		 WHERE kind = 'layout' AND is_deleted = 0 AND body != ''
		 ORDER BY updated_at DESC LIMIT 1`,
	).Scan(&fallbackBody)
	if err != nil || !fallbackBody.Valid || fallbackBody.String == "" {
		return nil
	}
	return &layoutData{Template: fallbackBody.String}
}

func readLayoutBody(projectDB *sql.DB, name string) string {
	if name == "" {
		return ""
	}
	var body sql.NullString
	err := projectDB.QueryRow(
		"SELECT body FROM of_files WHERE kind = 'layout' AND path = ? AND is_deleted = 0",
		name,
	).Scan(&body)
	if err != nil || !body.Valid {
		return ""
	}
	return body.String
}

// assetVersions maps each live file artifact's path to its cache-busting query
// suffix ("?v=<8 hex>"). /files is served with a day-long max-age, so a file's URL
// must change whenever its bytes do. The suffix is derived per file from its id,
// size, last write time and history version: a patch re-fetches only the module
// it touched, while every other URL stays cached. A path with no entry gets no
// suffix.
type assetVersions map[string]string

// loadAssetVersions reads the version suffix of every live file artifact. A failed
// query yields no suffixes, which disables cache-busting and nothing more.
func loadAssetVersions(projectDB *sql.DB) assetVersions {
	rows, err := projectDB.Query(
		`SELECT f.path, f.id, COALESCE(f.size,0), COALESCE(f.updated_at,''),
		        COALESCE((SELECT MAX(h.version_number) FROM of_file_history h WHERE h.kind = f.kind AND h.path = f.path), 0)
		   FROM of_files f
		  WHERE f.kind = 'file' AND f.is_deleted = 0`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := assetVersions{}
	for rows.Next() {
		var path, updated string
		var id, size, version int64
		if rows.Scan(&path, &id, &size, &updated, &version) != nil {
			continue
		}
		h := sha256.Sum256([]byte(fmt.Sprintf("%d|%d|%s|%d", id, size, updated, version)))
		out[path] = fmt.Sprintf("?v=%x", h[:4])
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// url is the versioned /files URL of path.
func (v assetVersions) url(path string) string {
	return "/files/" + path + v[path]
}

// serviceWorkerPath is the one platform-managed JS artifact the browser fetches
// itself via navigator.serviceWorker.register, not through a <script> tag or the
// importmap. Excluded from tag injection and from the project module map.
const serviceWorkerPath = "service-worker.js"

// autoInjectAssets queries global-scoped CSS/JS/favicon assets and returns
// link/script tags with versioned URLs. Every project JS file is an ES module;
// the browser de-dupes module loads by URL, so a tag for an also-imported file is
// harmless and is required when an entry sits in an import cycle.
func autoInjectAssets(projectDB *sql.DB, versions assetVersions) (cssLinks, jsScripts, iconLinks string) {
	rows, err := projectDB.Query(
		`SELECT path FROM of_files
		 WHERE kind = 'file' AND is_deleted = 0 AND scope = 'global'
		   AND (path LIKE '%.css' OR path LIKE '%.js' OR path LIKE '%.mjs'
		        OR path IN ('favicon.svg', 'favicon.png', 'favicon.ico', 'apple-touch-icon.png'))
		 ORDER BY path`,
	)
	if err != nil {
		return "", "", ""
	}
	defer rows.Close()

	var css, js, icons strings.Builder
	// Emit favicons in preference order: SVG -> ICO -> PNG, then apple-touch-icon.
	var hasSVG, hasICO, hasPNG, hasApple bool
	for rows.Next() {
		var fn string
		if rows.Scan(&fn) != nil {
			continue
		}
		lower := strings.ToLower(fn)
		switch {
		case strings.HasSuffix(lower, ".css"):
			css.WriteString(`  <link rel="stylesheet" href="` + versions.url(fn) + `">` + "\n")
		case strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs"):
			if fn == serviceWorkerPath {
				continue
			}
			js.WriteString(`  <script type="module" src="` + versions.url(fn) + `"></script>` + "\n")
		case lower == "favicon.svg":
			hasSVG = true
		case lower == "favicon.ico":
			hasICO = true
		case lower == "favicon.png":
			hasPNG = true
		case lower == "apple-touch-icon.png":
			hasApple = true
		}
	}
	// A failed iteration is handled like a failed query: no injected tags.
	if rows.Err() != nil {
		return "", "", ""
	}
	if hasSVG {
		icons.WriteString(`  <link rel="icon" type="image/svg+xml" href="` + versions.url("favicon.svg") + `">` + "\n")
	}
	if hasICO {
		icons.WriteString(`  <link rel="icon" type="image/x-icon" href="` + versions.url("favicon.ico") + `">` + "\n")
	}
	if hasPNG {
		icons.WriteString(`  <link rel="icon" type="image/png" href="` + versions.url("favicon.png") + `">` + "\n")
	}
	if hasApple {
		icons.WriteString(`  <link rel="apple-touch-icon" href="` + versions.url("apple-touch-icon.png") + `">` + "\n")
	}
	return css.String(), js.String(), icons.String()
}

// loadImportMap returns an HTML <script type="importmap"> for the page, merging
// two specifier sources: external kind="module" libraries from plan_json
// (module_name -> URL) and the project's own ES modules (logical name -> /files
// path, so siblings import by bare name). An explicit external library wins a
// basename clash. Empty when neither contributes; malformed plan_json drops those
// entries rather than blocking render.
func loadImportMap(projectDB *sql.DB, versions assetVersions) string {
	if projectDB == nil {
		return ""
	}
	imports := map[string]string{}
	// External module-kind libraries from the plan.
	var planJSON sql.NullString
	if err := projectDB.QueryRow(currentPlanJSONQuery).Scan(&planJSON); err == nil && planJSON.Valid && planJSON.String != "" {
		var shell struct {
			ExternalLibraries []struct {
				URL        string `json:"url"`
				Kind       string `json:"kind"`
				ModuleName string `json:"module_name"`
			} `json:"external_libraries"`
		}
		if json.Unmarshal([]byte(planJSON.String), &shell) == nil {
			for _, lib := range shell.ExternalLibraries {
				if !strings.EqualFold(strings.TrimSpace(lib.Kind), "module") {
					continue
				}
				spec := strings.TrimSpace(lib.ModuleName)
				url := strings.TrimSpace(lib.URL)
				if spec == "" || url == "" {
					continue
				}
				imports[spec] = url
			}
		}
	}
	// Internal project modules, skipping any name an external lib already claims.
	for name, url := range projectModuleImportEntries(projectModuleFilePaths(projectDB), versions) {
		if _, taken := imports[name]; !taken {
			imports[name] = url
		}
	}
	if len(imports) == 0 {
		return ""
	}
	body, err := json.Marshal(map[string]map[string]string{"imports": imports})
	if err != nil {
		return ""
	}
	return `  <script type="importmap">` + string(body) + "</script>\n"
}

// projectModuleFilePaths returns the paths of every internal JS module. All are
// importable by bare name through the importmap; only the service worker (fetched
// by the browser's registration machinery, never imported) is excluded.
func projectModuleFilePaths(projectDB *sql.DB) []string {
	rows, err := projectDB.Query(
		`SELECT path
		   FROM of_files
		  WHERE kind = 'file' AND is_deleted = 0
		    AND (path LIKE '%.js' OR path LIKE '%.mjs')`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) != nil {
			continue
		}
		if p == serviceWorkerPath {
			continue
		}
		paths = append(paths, p)
	}
	// A failed iteration is handled like a failed query: no module paths.
	if rows.Err() != nil {
		return nil
	}
	return paths
}

// projectModuleImportEntries maps each internal module path to its importmap
// entry: logical name -> "/files/<path>" plus that module's version suffix.
// Basename collisions are dropped (left to relative-path resolution). Pure, so
// unit-testable without a database.
//
// A versioned module also gets a URL-key entry mapping its unversioned /files URL
// to the versioned one, so a relative import resolves to the same versioned fetch
// as the bare form. Without it the browser would instantiate the module twice and
// live-binding singletons would fork. This also covers colliding-basename modules,
// which have no bare entry.
func projectModuleImportEntries(jsModulePaths []string, versions assetVersions) map[string]string {
	byName, _ := analysis.ProjectModuleMap(jsModulePaths)
	out := make(map[string]string, len(byName))
	for name, p := range byName {
		out[name] = versions.url(p)
	}
	for _, p := range jsModulePaths {
		if versions[p] != "" {
			out["/files/"+p] = versions.url(p)
		}
	}
	return out
}

// loadExternalLibTags reads plan.external_libraries and returns the <link>/<script>
// tags for every classic (non-module) library, so declared CDN assets load on every
// page even when the project stores no layout (the fallback shell gets them too).
// Render-time counterpart to loadImportMap (which wires module-kind libs). Empty when
// none exist; malformed plan_json produces no tags.
func loadExternalLibTags(projectDB *sql.DB, alreadyPresent string) string {
	if projectDB == nil {
		return ""
	}
	var planJSON sql.NullString
	err := projectDB.QueryRow(currentPlanJSONQuery).Scan(&planJSON)
	if err != nil || !planJSON.Valid || planJSON.String == "" {
		return ""
	}
	var shell struct {
		ExternalLibraries []externalLibSpec `json:"external_libraries"`
	}
	if err := json.Unmarshal([]byte(planJSON.String), &shell); err != nil {
		return ""
	}
	return buildExternalLibTags(shell.ExternalLibraries, alreadyPresent)
}

// externalLibSpec is the subset of a plan.external_libraries entry the renderer
// needs to emit a load tag.
type externalLibSpec struct {
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	ModuleName string `json:"module_name"`
	Integrity  string `json:"integrity"`
}

// buildExternalLibTags is the pure tag-shaping core of loadExternalLibTags, split
// out for testing. alreadyPresent is the layout template plus page markup; a
// library whose URL (or filename, to tolerate CDN-host swaps) already appears there
// is skipped so an existing tag is not loaded twice. Module-kind libs are emitted
// by loadImportMap and skipped here.
func buildExternalLibTags(libs []externalLibSpec, alreadyPresent string) string {
	lowerPresent := strings.ToLower(alreadyPresent)
	var out strings.Builder
	for _, lib := range libs {
		url := strings.TrimSpace(lib.URL)
		if url == "" {
			continue
		}
		// Module-kind libs resolve through the importmap (loadImportMap), not a tag.
		if strings.EqualFold(strings.TrimSpace(lib.Kind), "module") {
			continue
		}
		lowerURL := strings.ToLower(url)
		if strings.Contains(lowerPresent, lowerURL) {
			continue
		}
		fname := lowerURL
		if i := strings.LastIndex(fname, "/"); i >= 0 {
			fname = fname[i+1:]
		}
		if fname != "" && strings.Contains(lowerPresent, fname) {
			continue
		}
		// Cross-origin assets need crossorigin="anonymous" for SRI to take effect;
		// integrity= alone is silently ignored by browsers without it.
		extraAttrs := ""
		if analysis.CrossOriginURL(url) {
			extraAttrs = ` crossorigin="anonymous"`
			// integrity= is emitted only for assets whose bytes are stable. Font-
			// provider stylesheet endpoints content-negotiate on the User-Agent and
			// change over time, so a build-time hash never matches and the browser
			// blocks the load; suppress integrity for them (crossorigin stays, the
			// providers send ACAO: *).
			if integ := strings.TrimSpace(lib.Integrity); integ != "" && !analysis.ExternalURLIsDynamicCSS(url) {
				extraAttrs += ` integrity="` + integ + `"`
			}
		}
		switch {
		case analysis.ExternalURLIsCSS(url):
			out.WriteString(`  <link rel="stylesheet" href="` + url + `"` + extraAttrs + ">\n")
		case analysis.ExternalURLIsFontFile(url):
			// A raw font-file URL must never load as a <script>: the browser refuses
			// to execute it (strict MIME checking). A bare font can't apply itself
			// without an @font-face rule, so the safe maximum is a preload: no console
			// error, and it warms the cache for whatever @font-face references it.
			typeAttr := ""
			if t := analysis.FontFileMIMEType(url); t != "" {
				typeAttr = ` type="` + t + `"`
			}
			out.WriteString(`  <link rel="preload" href="` + url + `" as="font"` + typeAttr + extraAttrs + ">\n")
		case analysis.ExternalURLIsESM(url):
			// Defense-in-depth: a classic-declared ESM URL would throw as a plain
			// <script>, so emit type="module" in case an unvalidated spec reaches
			// the renderer.
			out.WriteString(`  <script type="module" src="` + url + `"` + extraAttrs + "></script>\n")
		default:
			out.WriteString(`  <script src="` + url + `"` + extraAttrs + "></script>\n")
		}
	}
	return out.String()
}

// loadPageAssetPaths returns the ordered list of artifact paths linked to a
// page via of_page_assets. Empty slice (not nil) if no links exist.
func loadPageAssetPaths(projectDB *sql.DB, pagePath string) []string {
	rows, err := projectDB.Query(
		`SELECT file_path FROM of_page_assets WHERE page_path = ? ORDER BY position, file_path`,
		pagePath,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	// A failed iteration is handled like a failed query: no linked assets.
	if rows.Err() != nil {
		return nil
	}
	return out
}

// splitAssetPathsByType groups artifact paths into CSS and JS URL lists for
// <link> and <script> tag emission. Non-CSS/JS paths are dropped (the page
// still references them via normal content attributes, e.g. <img src>).
func splitAssetPathsByType(paths []string) (cssURLs, jsURLs []string) {
	for _, p := range paths {
		lower := strings.ToLower(p)
		switch {
		case strings.HasSuffix(lower, ".css"):
			cssURLs = append(cssURLs, "/files/"+p)
		case strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".mjs"):
			jsURLs = append(jsURLs, "/files/"+p)
		}
	}
	return cssURLs, jsURLs
}

// injectPageAssets generates link/script tags for page-scoped artifacts with
// versioned URLs. Every attached JS file gets a <script type="module">; the
// browser de-dupes module loads by URL, so a tag for an also-imported file is
// harmless and is required when an attached entry sits in an import cycle.
func injectPageAssets(paths []string, versions assetVersions) (cssLinks, jsScripts string) {
	if len(paths) == 0 {
		return "", ""
	}

	var css, js strings.Builder
	for _, fn := range paths {
		if fn == "" {
			continue
		}
		lower := strings.ToLower(fn)
		if strings.HasSuffix(lower, ".css") {
			css.WriteString(`  <link rel="stylesheet" href="` + versions.url(fn) + `">` + "\n")
		} else if strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".mjs") {
			js.WriteString(`  <script type="module" src="` + versions.url(fn) + `"></script>` + "\n")
		}
	}
	return css.String(), js.String()
}

// documentSpec carries the inputs to renderDocument, grouping the adjacent
// path/title/content/metadata/layout strings that were easy to transpose.
type documentSpec struct {
	Project        *models.Project
	ProjectDB      *sql.DB
	PagePath       string
	Title          string
	Content        string
	MetadataJSON   string
	LayoutName     string
	PageAssetPaths []string
	RouteParams    map[string]string
}

func (h *Handler) renderDocument(spec documentSpec) string {
	project := spec.Project
	projectDB := spec.ProjectDB
	pagePath := spec.PagePath
	title := spec.Title
	content := spec.Content
	metadataJSON := spec.MetadataJSON
	layoutName := spec.LayoutName
	pageAssetPaths := spec.PageAssetPaths
	routeParams := spec.RouteParams

	// Parse page metadata.
	var meta pageMetadata
	if metadataJSON != "" && metadataJSON != "{}" {
		if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
			slog.Warn("failed to parse page metadata", "path", pagePath, "error", err)
		}
	}

	// Layer SEO overrides from of_seo_meta on top of page metadata. Non-empty
	// columns win; empty columns fall through so the engine can set just the
	// fields it cares about.
	var ogTitleOverride, ogDescriptionOverride string
	if seo := loadSEOMeta(projectDB, pagePath); seo != nil {
		ogTitleOverride, ogDescriptionOverride = seo.OGTitle, seo.OGDescription
		if seo.Title != "" {
			title = seo.Title
		}
		if seo.Description != "" {
			meta.Description = seo.Description
		}
		if seo.OGImage != "" {
			meta.OGImage = seo.OGImage
		}
		if seo.CanonicalURL != "" {
			meta.Canonical = seo.CanonicalURL
		}
		if seo.StructuredData != "" {
			meta.StructuredData = seo.StructuredData
		}
		if seo.TwitterCard != "" {
			meta.TwitterCard = seo.TwitterCard
		}
		if seo.Locale != "" {
			meta.Locale = seo.Locale
		}
	}

	// Resolve language/direction. Layouts own their own <html> tag so locale
	// reaches them only via og:locale; the fallback shell uses lang directly.
	lang := "en"
	if meta.Locale != "" {
		lang = meta.Locale
	}
	dir := "ltr"
	if isRTLLocale(lang) {
		dir = "rtl"
	}

	// Build the page title. A parameterized page (e.g. /blog/:slug) may carry
	// {param} placeholders, filled from the matched route params for a meaningful
	// first paint. Interpolate before escaping so URL-derived param values are
	// escaped along with the rest of the title.
	pageTitle := html.EscapeString(project.Name)
	if title != "" {
		pageTitle = html.EscapeString(interpolateRouteParams(title, routeParams))
	}

	// Build canonical URL.
	canonicalURL := meta.Canonical
	if canonicalURL == "" && project.Domain != nil && *project.Domain != "" {
		canonicalURL = "https://" + *project.Domain + pagePath
	}

	// Description.
	description := meta.Description

	// Load layout (NULL or empty -> "default", "none" -> no layout wrapping).
	// "none" pages skip the layout template entirely and use the synthesized fallback shell.
	var layout *layoutData
	noLayoutWrap := layoutName == "none"
	if layoutName == "" || layoutName == "none" {
		layoutName = "default"
	}
	layout = loadLayout(projectDB, layoutName)
	// Layouts may carry {{include:name}} too (the layout gate allows it), so
	// resolve them here or the placeholder ships to the browser as literal text.
	if layout != nil {
		layout.Template = resolveComponents(projectDB, layout.Template)
	}

	// Per-file cache-busting suffixes for every asset URL in this render, read once
	// and threaded into the asset helpers.
	versions := loadAssetVersions(projectDB)

	// Auto-inject global-scoped CSS/JS/favicon assets from the assets table.
	cssLinks, jsScripts, iconLinks := autoInjectAssets(projectDB, versions)

	// Page-scoped assets (only for this page), with version hash for cache-busting.
	pageCSSLinks, pageJSScripts := injectPageAssets(pageAssetPaths, versions)

	// Clean page content: strip any document-shell tags the LLM included,
	// drop duplicate asset references, resolve component placeholders, and
	// pull out page-specific <style>/<script> so the server can place them
	// at the correct spots in the final document.
	content = stripDocumentShell(content)
	content = stripSharedAssetRefs(content, projectDB)
	content = resolveComponents(projectDB, content)
	headTags, bodyEndTags, cleanContent := extractAssetTags(content)

	// Build the <head> injection blob: everything the server owns (title, meta, OG,
	// canonical, favicons, global + page-scoped CSS, rescued page <style>),
	// pre-indented for insertion into a user-authored template.
	var headBlob strings.Builder
	headBlob.WriteString(`  <meta charset="utf-8">` + "\n")
	headBlob.WriteString(`  <meta name="viewport" content="width=device-width, initial-scale=1">` + "\n")
	headBlob.WriteString(`  <meta name="generator" content="Open Fabrica">` + "\n")
	headBlob.WriteString("  <title>" + pageTitle + "</title>\n")
	// writeMeta emits `  <meta attr="key" content="...">` (content escaped) when
	// content is non-empty. attr is "name" or "property".
	writeMeta := func(attr, key, content string) {
		if content != "" {
			headBlob.WriteString(`  <meta ` + attr + `="` + key + `" content="` + html.EscapeString(content) + `">` + "\n")
		}
	}
	writeMeta("name", "description", description)
	writeMeta("name", "keywords", meta.Keywords)
	// An explicit og title/description (manage_seo set_meta og.title / og.description)
	// wins for link previews; the page title and description are the fallback.
	ogTitle := title
	if ogTitleOverride != "" {
		ogTitle = ogTitleOverride
	}
	ogDescription := description
	if ogDescriptionOverride != "" {
		ogDescription = ogDescriptionOverride
	}
	writeMeta("property", "og:title", ogTitle)
	writeMeta("property", "og:description", ogDescription)
	writeMeta("property", "og:image", meta.OGImage)
	if canonicalURL != "" {
		writeMeta("property", "og:url", canonicalURL)
		headBlob.WriteString(`  <link rel="canonical" href="` + html.EscapeString(canonicalURL) + `">` + "\n")
	}
	writeMeta("property", "og:locale", meta.Locale)
	// Twitter Card. Default to summary_large_image when an image is available,
	// summary otherwise. Explicit override from of_seo_meta wins.
	twitterCard := meta.TwitterCard
	if twitterCard == "" {
		if meta.OGImage != "" {
			twitterCard = "summary_large_image"
		} else {
			twitterCard = "summary"
		}
	}
	writeMeta("name", "twitter:card", twitterCard)
	writeMeta("name", "twitter:title", title)
	writeMeta("name", "twitter:description", description)
	writeMeta("name", "twitter:image", meta.OGImage)
	// JSON-LD structured data. Stored raw so the engine can author any
	// schema.org shape; we only require that it parses as JSON before
	// emitting, so a malformed value cannot break the page.
	if meta.StructuredData != "" {
		if json.Valid([]byte(meta.StructuredData)) {
			headBlob.WriteString(`  <script type="application/ld+json">` + meta.StructuredData + "</script>\n")
		} else {
			slog.Warn("invalid structured_data JSON; skipping", "path", pagePath)
		}
	}
	if iconLinks != "" {
		headBlob.WriteString(iconLinks)
	}
	// Importmap must appear before the first ES-module script. Module scripts
	// live in jsScripts/pageJSScripts (body tail) and possibly in user-authored
	// bodyEndTags; emitting in <head> guarantees document-order precedence.
	if importMap := loadImportMap(projectDB, versions); importMap != "" {
		headBlob.WriteString(importMap)
	}
	// Classic (non-module) external libraries from the plan: tags injected here so
	// they load whether the page uses a stored layout or the fallback shell below.
	// Deduped against the layout and page markup so an existing tag isn't loaded twice.
	var alreadyPresent strings.Builder
	if layout != nil {
		alreadyPresent.WriteString(layout.Template)
	}
	alreadyPresent.WriteString(headTags)
	alreadyPresent.WriteString(bodyEndTags)
	alreadyPresent.WriteString(cleanContent)
	if libTags := loadExternalLibTags(projectDB, alreadyPresent.String()); libTags != "" {
		headBlob.WriteString(libTags)
	}
	if cssLinks != "" {
		headBlob.WriteString(cssLinks)
	}
	if pageCSSLinks != "" {
		headBlob.WriteString(pageCSSLinks)
	}
	if headTags != "" {
		headBlob.WriteString(headTags)
		headBlob.WriteString("\n")
	}

	// Build the body-tail injection blob, route params, global + page JS,
	// rescued page <script>. Placed immediately before </body>.
	var bodyTail strings.Builder
	if len(routeParams) > 0 {
		paramsJSON, _ := json.Marshal(routeParams)
		bodyTail.WriteString(fmt.Sprintf("<script>window.__routeParams=%s;</script>\n", paramsJSON))
	} else {
		bodyTail.WriteString("<script>window.__routeParams={};</script>\n")
	}
	if jsScripts != "" {
		bodyTail.WriteString(jsScripts)
	}
	if pageJSScripts != "" {
		bodyTail.WriteString(pageJSScripts)
	}
	if bodyEndTags != "" {
		bodyTail.WriteString(bodyEndTags)
		bodyTail.WriteString("\n")
	}

	// Layout path: the template is a full HTML document. With a {{assets}} sink the
	// head blob substitutes it; without one it auto-inserts before </head>.
	// Body-tail always lands before </body>.
	if !noLayoutWrap && layout != nil && strings.Contains(layout.Template, "{{content}}") {
		// Drop any title/charset/viewport the layout authored; headBlob supplies
		// the canonical versions, and leaving the layout's copies in produces
		// duplicate <title>/charset/viewport tags in the final document.
		template := stripOwnedHeadTags(layout.Template)
		rendered := strings.Replace(template, "{{content}}", cleanContent, 1)
		if strings.Contains(rendered, "{{assets}}") {
			rendered = strings.ReplaceAll(rendered, "{{assets}}", headBlob.String())
		} else {
			rendered = insertBeforeTag(rendered, "</head>", headBlob.String())
		}
		rendered = insertBeforeTag(rendered, "</body>", bodyTail.String())
		return rendered
	}

	// Fallback path: no layout, or layout="none", or template missing {{content}}.
	// Synthesize a minimal shell around the cleaned page content.
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n")
	b.WriteString(`<html lang="` + html.EscapeString(lang) + `" dir="` + html.EscapeString(dir) + `">` + "\n")
	b.WriteString("<head>\n")
	b.WriteString(headBlob.String())
	b.WriteString("</head>\n")
	b.WriteString("<body>\n")
	b.WriteString(cleanContent)
	b.WriteString("\n")
	b.WriteString(bodyTail.String())
	b.WriteString("</body>\n</html>")
	return b.String()
}
