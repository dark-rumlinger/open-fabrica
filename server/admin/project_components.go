package admin

import (
	"errors"
	"fmt"
	"net/http"

	"openfabrica/store"
)

// ProjectComponentsHandler handles component listing/viewing/deletion for admin.
type ProjectComponentsHandler struct {
	deps *Deps
}

// List returns all components for a project.
func (h *ProjectComponentsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListComponents(projectDB))
}

// Get returns a single component by ID.
func (h *ProjectComponentsHandler) Get(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	componentID, ok := requireIntParam(w, r, "componentID", "invalid component ID")
	if !ok {
		return
	}
	c, err := store.GetComponent(projectDB, componentID)
	if err != nil {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// Delete removes a component by ID, unless pages or layouts reference it.
func (h *ProjectComponentsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	componentID, ok := requireIntParam(w, r, "componentID", "invalid component ID")
	if !ok {
		return
	}
	refCount, err := store.CountComponentReferences(projectDB, componentID)
	if errors.Is(err, store.ErrComponentNotFound) {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}
	if refCount > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("cannot delete: %d page(s)/layout(s) reference this component", refCount))
		return
	}
	n, err := softDeleteArtifact(projectDB, "component", componentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete component")
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": componentID})
}

// ListVersions returns version history for a component.
func (h *ProjectComponentsHandler) ListVersions(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	componentID, ok := requireIntParam(w, r, "componentID", "invalid component ID")
	if !ok {
		return
	}
	respondArtifactVersions(w, projectDB, "component", componentID)
}

// Restore reverts a component to a previous snapshotted body.
func (h *ProjectComponentsHandler) Restore(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	componentID, ok := requireIntParam(w, r, "componentID", "invalid component ID")
	if !ok {
		return
	}
	respondArtifactRestore(w, r, projectDB, "component", componentID)
}

type updateComponentRequest struct {
	Body *string `json:"body"`
}

// Update modifies a component's HTML body.
func (h *ProjectComponentsHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	componentID, ok := requireIntParam(w, r, "componentID", "invalid component ID")
	if !ok {
		return
	}
	var req updateComponentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Body == nil {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	err := store.UpdateComponentBody(projectDB, componentID, *req.Body)
	if errors.Is(err, store.ErrComponentNotFound) {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update component")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": componentID})
}
