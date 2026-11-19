package admin

import (
	"errors"
	"net/http"

	"openfabrica/store"
)

// ProjectAuthEndpointsHandler handles auth endpoint listing/deletion for admin.
type ProjectAuthEndpointsHandler struct {
	deps *Deps
}

// List returns all auth endpoints for a project.
func (h *ProjectAuthEndpointsHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListAuthEndpoints(projectDB, projectID))
}

// Delete removes an auth endpoint by ID.
func (h *ProjectAuthEndpointsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}
	err := store.DeleteAuthEndpoint(projectDB, endpointID)
	if errors.Is(err, store.ErrAuthEndpointNotFound) {
		writeError(w, http.StatusNotFound, "auth endpoint not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete auth endpoint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": endpointID})
}
