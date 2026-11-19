package public

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"openfabrica/db/models"
	"openfabrica/server/middleware"
	"openfabrica/store"
)

// Page serves page content as JSON for the given path query parameter.
// GET /api/page?path=/about
func (h *Handler) Page(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	projectDB := h.openProjectDB(w, project.ID)
	if projectDB == nil {
		return
	}

	pagePath := r.URL.Query().Get("path")
	if pagePath == "" {
		pagePath = "/"
	}

	// Check redirects for SPA navigation.
	if target, code, found := checkRedirect(projectDB.Reader(), pagePath); found {
		writeJSON(w, http.StatusOK, map[string]any{
			"redirect":    target,
			"status_code": code,
		})
		return
	}

	var content, title, metadata, layoutName sql.NullString
	var routeParams map[string]string
	resolvedPath := pagePath
	if pg, err := store.LoadPublishedPage(projectDB.Reader(), pagePath); err == nil {
		title, content, metadata, layoutName = pg.Title, pg.Content, pg.Metadata, pg.LayoutName
	} else {
		templatePath, params, ok := resolveParamPage(projectDB.Reader(), pagePath, &title, &content, &metadata, &layoutName)
		if !ok {
			writeError(w, http.StatusNotFound, "page not found")
			return
		}
		routeParams = params
		resolvedPath = templatePath
	}

	// Page-scope asset links come from of_page_assets (relational, renames
	// stay in sync, no JSON-in-column dangling references).
	pageAssetPaths := loadPageAssetPaths(projectDB.Reader(), resolvedPath)

	// Resolve {{include:component_name}} for SPA content too.
	resolvedContent := resolveComponents(projectDB.Reader(), content.String)

	// Strip shared asset refs the engine may have embedded, then strip external
	// scripts and CSS links for SPA, extracting their URLs so the router
	// can load them dynamically.
	cleaned := stripSharedAssetRefs(resolvedContent, projectDB.Reader())
	cleanContent, contentCSS, contentJS := stripForSPA(cleaned)

	// Normalize layout name for the SPA router (NULL/empty -> "default").
	respLayout := layoutName.String
	if respLayout == "" {
		respLayout = "default"
	}

	resp := map[string]any{
		"path":       pagePath,
		"title":      title.String,
		"content":    strings.TrimSpace(cleanContent),
		"metadata":   metadata.String,
		"project_id": project.ID,
		"layout":     respLayout,
	}
	if routeParams != nil {
		resp["params"] = routeParams
	}

	// Merge DB-declared page assets with URLs extracted from content.
	if len(pageAssetPaths) > 0 {
		dbCSS, dbJS := splitAssetPathsByType(pageAssetPaths)
		contentCSS = append(dbCSS, contentCSS...)
		contentJS = append(dbJS, contentJS...)
	}
	// Append each file's version suffix to its asset URL for cache-busting.
	versions := loadAssetVersions(projectDB.Reader())
	for _, urls := range [][]string{contentCSS, contentJS} {
		for i, url := range urls {
			if strings.HasPrefix(url, "/files/") && !strings.Contains(url, "?") {
				urls[i] = url + versions[strings.TrimPrefix(url, "/files/")]
			}
		}
	}
	if len(contentCSS) > 0 {
		resp["page_css"] = dedupStrings(contentCSS)
	}
	if len(contentJS) > 0 {
		resp["page_js"] = dedupStrings(contentJS)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

// SSR, Server-Side Rendered Pages

type pageMetadata struct {
	Description    string `json:"description"`
	OGImage        string `json:"og_image"`
	Canonical      string `json:"canonical"`
	Keywords       string `json:"keywords"`
	Locale         string `json:"locale"`
	TwitterCard    string `json:"twitter_card"`
	StructuredData string `json:"structured_data"`
}

// ServePage serves a fully-rendered HTML page by its URL path.
func (h *Handler) ServePage(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	projectDB := h.openProjectDB(w, project.ID)
	if projectDB == nil {
		return
	}

	pagePath := r.URL.Path
	if pagePath == "" {
		pagePath = "/"
	}

	// Check redirects before page lookup.
	if target, code, found := checkRedirect(projectDB.Reader(), pagePath); found {
		http.Redirect(w, r, target, code)
		return
	}

	var content, title, metadata, layoutName sql.NullString
	var routeParams map[string]string
	if pg, err := store.LoadPublishedPage(projectDB.Reader(), pagePath); err == nil {
		title, content, metadata, layoutName = pg.Title, pg.Content, pg.Metadata, pg.LayoutName
	} else {
		// Try common homepage variants: the engine often saves as /index.html instead of /
		found := false
		for _, alt := range homeFallbacks(pagePath) {
			if pg, err := store.LoadPublishedPage(projectDB.Reader(), alt); err == nil {
				title, content, metadata, layoutName = pg.Title, pg.Content, pg.Metadata, pg.LayoutName
				found = true
				pagePath = alt
				break
			}
		}
		if !found {
			if templatePath, params, ok := resolveParamPage(projectDB.Reader(), pagePath, &title, &content, &metadata, &layoutName); ok {
				found = true
				routeParams = params
				pagePath = templatePath
			}
		}
		if !found {
			h.serve404(w, project)
			return
		}
	}

	// Track analytics (fire-and-forget).
	go func() {
		defer func() {
			if rv := recover(); rv != nil {
				h.deps.Logger.Error("trackPageView panic", "error", rv, "project_id", project.ID)
			}
		}()
		h.trackPageView(project.ID, pagePath, r)
	}()

	// Render the full HTML document with layout wrapping.
	pageAssetPaths := loadPageAssetPaths(projectDB.Reader(), pagePath)
	doc := h.renderDocument(documentSpec{
		Project:        project,
		ProjectDB:      projectDB.Reader(),
		PagePath:       pagePath,
		Title:          title.String,
		Content:        content.String,
		MetadataJSON:   metadata.String,
		LayoutName:     layoutName.String,
		PageAssetPaths: pageAssetPaths,
		RouteParams:    routeParams,
	})
	// Under a strict, nonce-based CSP, stamp the per-request nonce onto inline
	// scripts so they run without 'unsafe-inline'. No-op on the permissive default
	// (NonceFromContext returns "").
	doc = stampInlineScriptNonce(doc, middleware.NonceFromContext(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(doc))
}

func (h *Handler) serve404(w http.ResponseWriter, project *models.Project) {
	// Check for a custom 404 page.
	projectDB, err := h.deps.ProjectDBManager.Open(project.ID)
	if err == nil {
		if pg, err := store.LoadPublishedPage(projectDB.Reader(), "/404"); err == nil {
			doc := h.renderDocument(documentSpec{
				Project:        project,
				ProjectDB:      projectDB.Reader(),
				PagePath:       "/404",
				Title:          pg.Title.String,
				Content:        pg.Content.String,
				MetadataJSON:   pg.Metadata.String,
				LayoutName:     pg.LayoutName.String,
				PageAssetPaths: loadPageAssetPaths(projectDB.Reader(), "/404"),
				RouteParams:    nil,
			})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(doc))
			return
		}

		// No content yet: show the "factory at work" placeholder instead of
		// a bare 404 when this project has zero published pages. Distinguishes
		// "site under construction" from "this URL is wrong".
		if pageCount, err := store.CountPublishedPages(projectDB.Reader()); err == nil && pageCount == 0 {
			// Light the matching public step on the placeholder. The current
			// stage is the latest of_runs row's stage. A read failure leaves
			// stage empty, which lights nothing rather than guessing.
			serveBuildingPage(w, store.LatestRunStage(projectDB.Reader()))
			return
		}
	}

	serveErrorPage(w, http.StatusNotFound, "Page Not Found", "The page you're looking for doesn't exist or has been moved.")
}

// checkRedirect looks up a redirect for the given path.
func checkRedirect(projectDB *sql.DB, path string) (target string, statusCode int, found bool) {
	return store.LookupRedirect(projectDB, path)
}

// homeFallbacks returns alternate paths to try when an exact page path isn't found.
// This handles the common case where the engine saves the homepage as "/index.html"
// but the browser requests "/", or vice versa.
func homeFallbacks(path string) []string {
	switch path {
	case "/":
		return []string{"/index.html", "/index", "/home"}
	case "/index.html", "/index":
		return []string{"/"}
	default:
		// No fallback for non-root paths. Each route must be a real page.
		// SPA client-side routing handles transitions; the server serves
		// each route individually for SSR/SEO.
		return nil
	}
}

func resolveParamPage(projectDB *sql.DB, requestPath string, title, content, metadata, layoutName *sql.NullString) (string, map[string]string, bool) {
	templatePath, params, err := findParamPage(projectDB, requestPath)
	if err != nil {
		return "", nil, false
	}
	pg, err := store.LoadPublishedPage(projectDB, templatePath)
	if err != nil {
		return "", nil, false
	}
	*title, *content, *metadata, *layoutName = pg.Title, pg.Content, pg.Metadata, pg.LayoutName
	return templatePath, params, true
}

// findParamPage matches a request path against parameterized page templates, e.g.
// "/thread/4" matches "/thread/:id" -> {"id": "4"}.
//
// When several templates match, the MOST SPECIFIC one wins (Express-style: a literal
// segment beats a ':param' at the same position, left to right). Without this,
// "/alice/about" would match both "/:user/:slug" and "/:user/about" with the winner
// depending on DB row order - so a post route ":slug" could shadow static routes like
// /about or /dashboard and serve them as a missing post.
func findParamPage(projectDB *sql.DB, requestPath string) (templatePath string, params map[string]string, err error) {
	candidates, err := store.ListParamPagePaths(projectDB)
	if err != nil {
		return "", nil, err
	}

	reqParts := strings.Split(requestPath, "/")

	bestPath := ""
	var bestParts []string
	var bestParams map[string]string

	for _, candidate := range candidates {

		candParts := strings.Split(candidate, "/")
		if len(candParts) != len(reqParts) {
			continue
		}

		match := true
		extracted := map[string]string{}
		for i, cp := range candParts {
			if strings.HasPrefix(cp, ":") {
				extracted[cp[1:]] = reqParts[i]
			} else if cp != reqParts[i] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		// Keep the most specific match, not merely the first one DB order returns.
		if bestPath == "" || moreSpecificRoute(candParts, bestParts) {
			bestPath = candidate
			bestParts = candParts
			bestParams = extracted
		}
	}
	if bestPath == "" {
		return "", nil, fmt.Errorf("no matching parameterized page")
	}
	return bestPath, bestParams, nil
}

// moreSpecificRoute reports whether route a is strictly more specific than route
// b. Both are already known to match the same request, so they have equal segment
// counts and any literal segment equals the request's. Specificity is decided
// segment by segment, left to right: at the first position where one route has a
// literal and the other a ':param', the literal route is more specific (static
// beats dynamic - the rule every conventional router uses).
func moreSpecificRoute(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		aParam := strings.HasPrefix(a[i], ":")
		bParam := strings.HasPrefix(b[i], ":")
		if aParam != bParam {
			return !aParam // a is literal here, b is a param -> a wins
		}
	}
	return false
}

// visitorHash is the stable, anonymous id analytics counts unique visitors by. It
// is keyed with the instance secret: a bare hash of ip+user-agent can be reversed
// by hashing the whole IPv4 space against a leaked analytics table, while the
// keyed form cannot be recomputed without the secret. The "visitor|" prefix keeps
// it separate from the other uses of that key.
func (h *Handler) visitorHash(ip, userAgent string) string {
	if h.deps.JWTManager == nil {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(ip+userAgent)))[:16]
	}
	return fmt.Sprintf("%x", h.deps.JWTManager.HMAC([]byte("visitor|"+ip+"|"+userAgent)))[:16]
}

func (h *Handler) trackPageView(projectID int, pagePath string, r *http.Request) {
	projectDB, err := h.deps.ProjectDBManager.Open(projectID)
	if err != nil {
		return
	}

	ip := middleware.ClientIP(r)
	visitorHash := h.visitorHash(ip, r.UserAgent())
	referrer := r.Referer()
	ua := r.UserAgent()

	store.RecordPageView(projectDB, pagePath, visitorHash, referrer, ua)
}
