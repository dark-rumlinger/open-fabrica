package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"openfabrica/store"
)

// ServiceProvidersHandler handles service provider listing for the admin UI.
type ServiceProvidersHandler struct {
	deps *Deps
}

// List returns service providers for a project.
func (h *ServiceProvidersHandler) List(w http.ResponseWriter, r *http.Request) {
	projectIDStr := chi.URLParam(r, "projectID")
	if projectIDStr == "" {
		projectIDStr = r.URL.Query().Get("project_id")
	}
	projectID, err := strconv.Atoi(projectIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project ID")
		return
	}

	projectDB := openProjectDB(w, h.deps.ProjectDBManager, projectID)
	if projectDB == nil {
		return
	}
	providers, err := store.ListServiceProviders(projectDB, projectID)
	if err != nil {
		h.deps.serverError(w, "failed to list service providers", err)
		return
	}
	writeJSON(w, http.StatusOK, providers)
}

// Toggle flips the is_enabled flag on a service provider.
func (h *ServiceProvidersHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	provID, ok := requireIntParam(w, r, "provID", "invalid provider ID")
	if !ok {
		return
	}
	if err := store.ToggleServiceProvider(projectDB, provID); err != nil {
		h.deps.Logger.Error("failed to toggle service provider", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to toggle provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": provID, "toggled": true})
}

// Delete removes a service provider.
func (h *ServiceProvidersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	provID, ok := requireIntParam(w, r, "provID", "invalid provider ID")
	if !ok {
		return
	}
	err := store.DeleteServiceProvider(projectDB, provID)
	if errors.Is(err, store.ErrServiceProviderNotFound) {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err != nil {
		h.deps.Logger.Error("failed to delete service provider", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete provider")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
