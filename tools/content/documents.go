package content

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"openfabrica/store"
	"openfabrica/tools"
)

// Implementation for manage_documents. Renders a project page or an inline HTML
// string to PDF via the shared headless-Chrome launcher and stores the result
// as a kind=file artifact served at /files/<name>, the same surface generated
// images use. No external provider is required; the only dependency is a
// Chrome/Edge binary on the host, and its absence is surfaced as a clear
// browser_unavailable error rather than a silent miss.

const (
	pdfContentType = "application/pdf"
	// maxRenderHTMLBytes caps inline HTML so a runaway payload can't be pushed
	// into a headless render. 2MB is far above any realistic single document.
	maxRenderHTMLBytes = 2 << 20
)

// filenameSanitizeRe strips anything that isn't a safe filename character so a
// caller-supplied name can't escape the assets directory or collide with path
// separators. Applied after taking path.Base, belt-and-suspenders.
var filenameSanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// docResolveDomain returns the project's configured vhost and the public port,
// mirroring observability.testResolveDomain. A render_url needs the project's
// origin so its own CSS/JS/asset URLs resolve against the running public server.
func docResolveDomain(ctx *tools.ToolContext) (string, int, error) {
	port := ctx.PublicPortOrDefault()
	var domain sql.NullString
	if ctx.GlobalDB != nil {
		_ = ctx.GlobalDB.QueryRow("SELECT domain FROM projects WHERE id = ?", ctx.ProjectID).Scan(&domain)
	}
	if !domain.Valid || domain.String == "" {
		return "", 0, fmt.Errorf("no domain configured for this project; render_url needs the project's vhost to resolve its pages and assets. Use render_html for a standalone document, or set a domain on the project")
	}
	return domain.String, port, nil
}

// sanitizePDFFilename normalises a caller-supplied filename to a safe basename
// ending in .pdf, falling back to a default when empty.
func sanitizePDFFilename(name, fallback string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = fallback
	}
	name = path.Base(name)
	name = strings.TrimSuffix(name, ".pdf")
	name = filenameSanitizeRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-._")
	if name == "" {
		name = fallback
	}
	return name + ".pdf"
}

// pdfOptsFrom builds store.PDFOptions from the action args common to both render
// paths. printBackground defaults to true; the arg inverts into DisableBackground.
func pdfOptsFrom(pageSize string, landscape, printBackground bool, marginInches float64) store.PDFOptions {
	return store.PDFOptions{
		PageSize:          pageSize,
		Landscape:         landscape,
		DisableBackground: !printBackground,
		MarginInches:      marginInches,
	}
}

// saveAndReport stores rendered PDF bytes as an artifact and builds the success
// result. Shared by both render paths so they report identically.
func saveAndReport(ctx *tools.ToolContext, data []byte, filename, source string) (*tools.Result, error) {
	if len(data) == 0 {
		return tools.Fail("PDF render produced no bytes"), nil
	}
	if _, err := SaveBytesAsArtifact(ctx, data, pdfContentType, filename, map[string]any{"source": source}); err != nil {
		return tools.Fail("saving document: %s", err.Error()), nil
	}
	return tools.Ok(map[string]any{
		"path":         filename,
		"url":          "/files/" + filename,
		"content_type": pdfContentType,
		"size":         len(data),
	}), nil
}

// browserFail converts a LaunchBrowser/render error into the right result: a
// stable browser_unavailable code when Chrome is missing (so the LLM knows it's
// an environment gap, not a project bug), otherwise a plain failure.
func browserFail(err error) *tools.Result {
	if errors.Is(err, store.ErrBrowserUnavailable) {
		return tools.FailCode("browser_unavailable",
			"%v. PDF generation needs a Chrome/Edge binary on the host; install one so chromedp can launch", err)
	}
	return tools.FailErr(err)
}

func documentRenderURL(ctx *tools.ToolContext, urlPath, filename, pageSize string, landscape, printBackground bool, marginInches float64) (*tools.Result, error) {
	urlPath = strings.TrimSpace(urlPath)
	if urlPath == "" {
		return tools.Fail("page_route is required (the page to render, e.g. \"/invoice\")"), nil
	}
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}

	domain, port, err := docResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}

	fallback := sanitizePDFFilename(strings.ReplaceAll(strings.Trim(urlPath, "/"), "/", "-"), "document")
	out := sanitizePDFFilename(filename, fallback)

	if status := docRouteStatus(ctx, domain, port, urlPath); status == http.StatusNotFound {
		return tools.FailCode(tools.CodeNotFound, "page_route %s answers 404, so the PDF would be the not-found page - render an existing page route (manage_files list kind=page)", urlPath), nil
	}

	url := fmt.Sprintf("http://%s:%d%s", domain, port, urlPath)
	cfg := store.BrowserConfig{HostResolverMap: domain}
	data, err := store.RenderURLToPDF(ctx.Context(), cfg, url, pdfOptsFrom(pageSize, landscape, printBackground, marginInches))
	if err != nil {
		return browserFail(err), nil
	}
	return saveAndReport(ctx, data, out, "url:"+urlPath)
}

func documentRenderHTML(ctx *tools.ToolContext, html, filename, pageSize string, landscape, printBackground bool, marginInches float64) (*tools.Result, error) {
	if strings.TrimSpace(html) == "" {
		return tools.Fail("html is required"), nil
	}
	if len(html) > maxRenderHTMLBytes {
		return tools.Fail("html exceeds %d byte cap (got %d); render a project page with render_url for large documents", maxRenderHTMLBytes, len(html)), nil
	}

	out := sanitizePDFFilename(filename, "document")
	// HTML renders on about:blank, so a project vhost is optional here.
	domain, _, _ := docResolveDomain(ctx)
	cfg := store.BrowserConfig{}
	if domain != "" {
		cfg.HostResolverMap = domain
	}
	data, err := store.RenderHTMLToPDF(ctx.Context(), cfg, html, pdfOptsFrom(pageSize, landscape, printBackground, marginInches))
	if err != nil {
		return browserFail(err), nil
	}
	return saveAndReport(ctx, data, out, "html")
}

func documentList(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		`SELECT path, size FROM of_files
		 WHERE kind = 'file' AND content_type = ? AND is_deleted = 0
		 ORDER BY updated_at DESC`, pdfContentType)
	if err != nil {
		return tools.Fail("listing documents: %s", err.Error()), nil
	}
	defer rows.Close()
	docs := []map[string]any{}
	for rows.Next() {
		var p string
		var size int
		if rows.Scan(&p, &size) == nil {
			docs = append(docs, map[string]any{
				"path": p,
				"url":  "/files/" + p,
				"size": size,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("listing documents: %s", err.Error()), nil
	}
	return tools.Ok(map[string]any{"documents": docs}), nil
}

// docRouteStatus asks the project's public server for a route before rendering it,
// so a route that does not exist fails by name instead of printing the 404 page.
// Returns 0 when the server cannot be reached (the render then reports its own error).
func docRouteStatus(ctx *tools.ToolContext, domain string, port int, route string) int {
	req, err := http.NewRequestWithContext(ctx.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, route), nil)
	if err != nil {
		return 0
	}
	req.Host = domain
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
