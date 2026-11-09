package public

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openfabrica/internal/httpx"
	"openfabrica/store"
)

// Sitemap

func (h *Handler) Sitemap(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	projectDB := h.openProjectDB(w, project.ID)
	if projectDB == nil {
		return
	}

	baseURL := "https://" + r.Host
	if project.Domain != nil && *project.Domain != "" {
		baseURL = "https://" + *project.Domain
	}

	rows, err := projectDB.Query(
		"SELECT path, updated_at FROM of_files WHERE kind = 'page' AND status = 'published' AND is_deleted = 0 ORDER BY path",
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load sitemap")
		return
	}
	defer rows.Close()

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")

	for rows.Next() {
		var path string
		var updatedAt time.Time
		if err := rows.Scan(&path, &updatedAt); err != nil {
			continue
		}
		// A parameterized route (/blog/:slug) is a pattern, not a URL a crawler can
		// fetch; listing it literally would advertise a dead address.
		if strings.ContainsAny(path, ":{") {
			continue
		}
		b.WriteString("  <url>\n")
		b.WriteString("    <loc>" + html.EscapeString(baseURL+path) + "</loc>\n")
		b.WriteString("    <lastmod>" + updatedAt.Format("2006-01-02") + "</lastmod>\n")
		b.WriteString("  </url>\n")
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load sitemap")
		return
	}

	b.WriteString("</urlset>\n")

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(b.String()))
}

// Robots.txt

func (h *Handler) Robots(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	baseURL := "https://" + r.Host
	if project != nil && project.Domain != nil && *project.Domain != "" {
		baseURL = "https://" + *project.Domain
	}

	// A custom robots.txt set via `manage_seo set_robots` is stored under the
	// project's files dir; serve it when present. Fall back to a permissive
	// default (pointing crawlers at the sitemap) when the owner hasn't set one.
	if project != nil {
		robotsPath := filepath.Join(h.deps.ProjectDBManager.ProjectDir(project.ID), "files", "robots.txt")
		if data, err := os.ReadFile(robotsPath); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
	}

	body := "User-agent: *\nAllow: /\nSitemap: " + baseURL + "/sitemap.xml\n"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(body))
}

// PWA Assets (manifest + service worker served from DB content)

func (h *Handler) ServeManifest(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		http.NotFound(w, r)
		return
	}

	projectDB, err := h.deps.ProjectDBManager.Open(project.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var storage, body, diskPath sql.NullString
	err = projectDB.QueryRow(
		"SELECT storage, body, disk_path FROM of_files WHERE kind = 'file' AND path = 'manifest.json' AND is_deleted = 0",
	).Scan(&storage, &body, &diskPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, readErr := store.ReadArtifactBytes(storage, body, diskPath, h.deps.ProjectDBManager.ProjectDir(project.ID))
	if readErr != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (h *Handler) ServeServiceWorker(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		http.NotFound(w, r)
		return
	}

	projectDB, err := h.deps.ProjectDBManager.Open(project.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var storage, body, swPath sql.NullString
	err = projectDB.QueryRow(
		"SELECT storage, body, disk_path FROM of_files WHERE kind = 'file' AND path = 'service-worker.js' AND is_deleted = 0",
	).Scan(&storage, &body, &swPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	swData, readErr := store.ReadArtifactBytes(storage, body, swPath, h.deps.ProjectDBManager.ProjectDir(project.ID))
	if readErr != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write(swData)
}

// Artifact Serving, one handler for every file served from /files/<path>.
// Text bytes live in body (storage="inline"), binary on disk (storage="disk").

// defaultFaviconSVG is a 32x32 generic mark served when a project has no
// favicon of its own. Without this, every fresh project shows a noisy
// "favicon.ico 404" in the browser console, confusing for owners and
// distracting in screenshots/demos. Page <link rel="icon"> tags injected
// by render.go still take precedence when a real favicon is saved.
const defaultFaviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="6" fill="#111"/><circle cx="16" cy="16" r="6" fill="#fff"/></svg>`

// ServeFavicon handles the browser's automatic /favicon.ico request. Browsers fetch
// this URL unconditionally even when the HTML has a <link rel="icon">, so a route is
// still needed, but the actual bytes are served by ServeFile at /files/favicon.{ext}
// for proper ETag + immutable caching. It redirects to the saved file when one exists;
// otherwise it serves an embedded default SVG (no 404 noise).
//
// Resolution order: favicon.ico -> favicon.svg -> favicon.png -> embedded default.
func (h *Handler) ServeFavicon(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		serveDefaultFavicon(w)
		return
	}

	projectDB, err := h.deps.ProjectDBManager.Open(project.ID)
	if err != nil {
		serveDefaultFavicon(w)
		return
	}

	var filename string
	err = projectDB.QueryRow(
		`SELECT path FROM of_files
		 WHERE kind = 'file' AND is_deleted = 0
		   AND path IN ('favicon.ico', 'favicon.svg', 'favicon.png')
		 ORDER BY CASE path
		   WHEN 'favicon.ico' THEN 1
		   WHEN 'favicon.svg' THEN 2
		   ELSE 3
		 END
		 LIMIT 1`,
	).Scan(&filename)
	if err != nil || filename == "" {
		serveDefaultFavicon(w)
		return
	}

	http.Redirect(w, r, "/files/"+filename, http.StatusMovedPermanently)
}

// serveDefaultFavicon writes the embedded fallback SVG with a short cache
// window. The short max-age means the browser will pick up a real favicon
// shortly after the LLM saves one, no manual cache-bust needed.
func serveDefaultFavicon(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(defaultFaviconSVG))
}

// mimeByExtension returns the correct MIME type for common web file extensions.
// This is the source of truth, the DB content_type may be wrong if the LLM set it incorrectly.
func mimeByExtension(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json"
	case ".html", ".htm":
		return "text/html"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".ico":
		return "image/x-icon"
	case ".xml":
		return "application/xml"
	case ".pdf":
		return "application/pdf"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	default:
		return ""
	}
}

// ServeFile serves everything under /files/<path>, every kind='file'
// row, regardless of whether it was engine-authored (CSS/JS), user-uploaded
// (images/PDFs), or platform-generated (manifest.json, service-worker.js,
// api-docs.json).
func (h *Handler) ServeFile(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		http.NotFound(w, r)
		return
	}

	filename := strings.TrimPrefix(r.URL.Path, "/files/")
	if filename == "" {
		http.NotFound(w, r)
		return
	}
	cleaned := filepath.ToSlash(filepath.Clean(filename))
	if cleaned != filename || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
		http.NotFound(w, r)
		return
	}

	projectDB, err := h.deps.ProjectDBManager.Open(project.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var storage, body, diskPath, contentType sql.NullString
	err = projectDB.QueryRow(
		"SELECT storage, body, disk_path, content_type FROM of_files WHERE kind = 'file' AND path = ? AND is_deleted = 0",
		filename,
	).Scan(&storage, &body, &diskPath, &contentType)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	data, err := store.ReadArtifactBytes(storage, body, diskPath, h.deps.ProjectDBManager.ProjectDir(project.ID))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ct := mimeByExtension(filename)
	if ct == "" {
		ct = contentType.String
	}
	if ct == "" {
		ct = "application/octet-stream"
	}

	// Never let the browser sniff a different type than we declare: a sniffed
	// type can turn an inert upload into active content.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// User uploads live under "uploads/" and are untrusted end-user bytes.
	// Serving an uploaded .svg/.html inline, same-origin, is stored XSS. Force
	// any script-capable type to download instead of render. Engine-authored
	// assets (CSS/JS/etc., never under uploads/) are trusted app code and stay
	// inline.
	if strings.HasPrefix(filename, "uploads/") && isActiveContentType(ct) {
		w.Header().Set("Content-Disposition", "attachment")
		ct = "application/octet-stream"
	}

	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))[:18] + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400, must-revalidate")

	// A large file over a slow link outlives the server's blanket write timeout.
	httpx.ExtendIO(w, fileServeTimeout)

	// ServeContent answers conditional (ETag) and Range requests: Safari will not
	// play <video>/<audio> from a server that ignores Range.
	http.ServeContent(w, r, filename, time.Time{}, bytes.NewReader(data))
}

// fileServeTimeout bounds how long one /files/ response may take to write.
const fileServeTimeout = 10 * time.Minute

// isActiveContentType reports whether a content type can execute script when a
// browser renders it inline, same-origin (HTML, SVG, XML/XHTML, JS). Untrusted
// user uploads of these types are served as downloads to neutralise stored XSS.
func isActiveContentType(ct string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch base {
	case "text/html", "application/xhtml+xml", "image/svg+xml",
		"text/xml", "application/xml", "text/javascript", "application/javascript":
		return true
	}
	return false
}
