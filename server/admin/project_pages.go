package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"openfabrica/store"
)

// ProjectPagesHandler handles page listing for project detail views.
type ProjectPagesHandler struct {
	deps *Deps
}

// List returns all pages for a project (without content to keep response small).
func (h *ProjectPagesHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListPages(projectDB, projectID))
}

// Get returns a single page with its content.
func (h *ProjectPagesHandler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}
	p, err := store.GetPage(projectDB, projectID, pageID)
	if errors.Is(err, store.ErrPageNotFound) {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load page")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type createPageRequest struct {
	Path    string  `json:"path"`
	Title   *string `json:"title"`
	Content *string `json:"content"`
}

// Create inserts a new page.
func (h *ProjectPagesHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	var req createPageRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	if !strings.HasPrefix(req.Path, "/") {
		req.Path = "/" + req.Path
	}
	req.Path = strings.TrimRight(req.Path, "/")
	if strings.Contains(req.Path, "..") {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	id, err := store.CreatePage(projectDB, req.Path, req.Title, req.Content)
	if errors.Is(err, store.ErrPageDuplicate) {
		writeError(w, http.StatusConflict, fmt.Sprintf("page %q already exists", req.Path))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create page")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "path": req.Path})
}

type updatePageRequest struct {
	Title   *string `json:"title"`
	Content *string `json:"content"`
	Status  *string `json:"status"`
}

// Update modifies a page's content, title, or status.
func (h *ProjectPagesHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}
	var req updatePageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == nil && req.Content == nil && req.Status == nil {
		writeError(w, http.StatusBadRequest, "no fields to update")
		return
	}
	err := store.UpdatePage(projectDB, pageID, store.PageUpdate{
		Title:   req.Title,
		Content: req.Content,
		Status:  req.Status,
	})
	if errors.Is(err, store.ErrPageNotFound) {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update page")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": pageID})
}

// Delete soft-deletes a page.
func (h *ProjectPagesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}
	n, err := softDeleteArtifact(projectDB, "page", pageID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete page")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "page not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": pageID})
}

// ListVersions returns version history for a page.
func (h *ProjectPagesHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}
	respondArtifactVersions(w, projectDB, "page", pageID)
}

// Restore reverts a page to a previous snapshotted body.
func (h *ProjectPagesHandler) Restore(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	pageID, ok := requireIntParam(w, r, "pageID", "invalid page ID")
	if !ok {
		return
	}
	respondArtifactRestore(w, r, projectDB, "page", pageID)
}
