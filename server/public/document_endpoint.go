package public

// Runtime PDF endpoint: POST /api/<path>/pdf with {"data": {...}} fills the
// endpoint's stored HTML template and answers the rendered PDF. The template is
// project-authored; the request data is only ever substituted as escaped text and
// the render runs with scripts off, so a visitor can shape the content of a
// document but never inject markup or code into it. Rendering starts a headless
// browser, so it is closed by default, rate limited, and bounded to a couple of
// concurrent renders per process.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/doctemplate"
	"openfabrica/server/middleware"
	"openfabrica/store"
)

const (
	maxDocumentBodyBytes   = 256 << 10 // request data is rows of an order, not a file
	maxConcurrentRenders   = 2
	documentQueueWait      = 10 * time.Second
	defaultDocumentLimit   = 10 // requests/min per visitor
	documentFilenameMaxLen = 80
)

// renderSlots bounds simultaneous headless browsers across all projects.
var renderSlots = make(chan struct{}, maxConcurrentRenders)

var documentFilenameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type documentEndpoint struct {
	Template     string
	Filename     string
	PageSize     string
	Landscape    bool
	RequiresAuth bool
	RateLimit    int
}

func loadDocumentEndpoint(db *sql.DB, path string) (*documentEndpoint, error) {
	var ep documentEndpoint
	err := db.QueryRow(
		`SELECT template_html, filename, page_size, COALESCE(landscape,0), COALESCE(requires_auth,1), COALESCE(rate_limit,?)
		   FROM of_document_endpoints WHERE path = ?`, defaultDocumentLimit, path,
	).Scan(&ep.Template, &ep.Filename, &ep.PageSize, &ep.Landscape, &ep.RequiresAuth, &ep.RateLimit)
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// documentFilename renders the endpoint's filename pattern from the request data
// and reduces it to a safe basename ending in .pdf.
func documentFilename(pattern string, data map[string]any) string {
	name, err := doctemplate.Render(pattern, data)
	if err != nil || strings.TrimSpace(name) == "" {
		name = "document"
	}
	// Rendered values are HTML-escaped; for a filename only the safe subset survives.
	name = documentFilenameRe.ReplaceAllString(name, "-")
	name = strings.Trim(strings.TrimSuffix(name, ".pdf"), "-._")
	if len(name) > documentFilenameMaxLen {
		name = name[:documentFilenameMaxLen]
	}
	if name == "" {
		name = "document"
	}
	return name + ".pdf"
}

func (h *Handler) apiDocument(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *db.ProjectDB, path string) {
	ep, err := loadDocumentEndpoint(projectDB.Reader(), path)
	if err != nil {
		writeError(w, http.StatusNotFound, "document endpoint not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if ep.RequiresAuth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !h.validateProjectTokenOrJWT(project.ID, token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}
	if !checkEndpointRateLimit(fmt.Sprintf("doc:%d:%s:%s", project.ID, path, middleware.RateLimitIdentity(r)), ep.RateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	var body struct {
		Data map[string]any `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxDocumentBodyBytes+1))
	if err != nil || len(raw) > maxDocumentBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request data is too large")
		return
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	if body.Data == nil {
		body.Data = map[string]any{}
	}

	html, err := doctemplate.Render(ep.Template, body.Data)
	if err != nil {
		if errors.Is(err, doctemplate.ErrTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "the document is too large")
		} else {
			slog.Warn("document template failed", "project_id", project.ID, "path", path, "error", err)
			writeError(w, http.StatusInternalServerError, "the document template is invalid")
		}
		return
	}

	select {
	case renderSlots <- struct{}{}:
		defer func() { <-renderSlots }()
	case <-time.After(documentQueueWait):
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, "too many documents are being rendered, try again shortly")
		return
	case <-r.Context().Done():
		return
	}

	pdf, err := store.RenderHTMLToPDF(r.Context(), store.BrowserConfig{}, html, store.PDFOptions{
		PageSize:       ep.PageSize,
		Landscape:      ep.Landscape,
		DisableScripts: true,
	})
	if err != nil {
		if errors.Is(err, store.ErrBrowserUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "PDF rendering needs Chrome or Edge installed on the server")
			return
		}
		slog.Warn("document render failed", "project_id", project.ID, "path", path, "error", err)
		writeError(w, http.StatusInternalServerError, "could not render the document")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", documentFilename(ep.Filename, body.Data)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf)
}
