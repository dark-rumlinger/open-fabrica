package admin

import (
	"errors"
	"fmt"
	"net/http"

	"openfabrica/store"
)

// ProjectLayoutsHandler handles layout listing/viewing/deletion for admin.
type ProjectLayoutsHandler struct {
	deps *Deps
}

// List returns all layouts for a project.
func (h *ProjectLayoutsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListLayouts(projectDB))
}

// Get returns a single layout by ID.
func (h *ProjectLayoutsHandler) Get(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	layoutID, ok := requireIntParam(w, r, "layoutID", "invalid layout ID")
	if !ok {
		return
	}
	l, err := store.GetLayout(projectDB, layoutID)
	if err != nil {
		writeError(w, http.StatusNotFound, "layout not found")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// Delete removes a layout by ID, unless pages reference it.
func (h *ProjectLayoutsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	layoutID, ok := requireIntParam(w, r, "layoutID", "invalid layout ID")
	if !ok {
		return
	}
	pageCount, err := store.CountLayoutReferences(projectDB, layoutID)
	if errors.Is(err, store.ErrLayoutNotFound) {
		writeError(w, http.StatusNotFound, "layout not found")
		return
	}
	if pageCount > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("cannot delete: %d page(s) use this layout", pageCount))
		return
	}
	n, err := softDeleteArtifact(projectDB, "layout", layoutID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete layout")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "layout not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": layoutID})
}

// ListVersions returns version history for a layout.
func (h *ProjectLayoutsHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	layoutID, ok := requireIntParam(w, r, "layoutID", "invalid layout ID")
	if !ok {
		return
	}
	respondArtifactVersions(w, projectDB, "layout", layoutID)
}

// Restore reverts a layout to a previous snapshotted body.
func (h *ProjectLayoutsHandler) Restore(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	layoutID, ok := requireIntParam(w, r, "layoutID", "invalid layout ID")
	if !ok {
		return
	}
	respondArtifactRestore(w, r, projectDB, "layout", layoutID)
}

type updateLayoutRequest struct {
	Template *string `json:"template"`
}

// Update modifies a layout's template.
func (h *ProjectLayoutsHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	layoutID, ok := requireIntParam(w, r, "layoutID", "invalid layout ID")
	if !ok {
		return
	}
	var req updateLayoutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Template == nil {
		writeError(w, http.StatusBadRequest, "template is required")
		return
	}
	err := store.UpdateLayoutTemplate(projectDB, layoutID, *req.Template)
	if errors.Is(err, store.ErrLayoutNotFound) {
		writeError(w, http.StatusNotFound, "layout not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update layout")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": layoutID})
}
