package admin

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

// ServiceProvidersHandler handles service provider listing for the admin UI.
type ServiceProvidersHandler struct {
	deps *Deps
}

type serviceProvider struct {
	ID          int       `json:"id"`
	ProjectID   int       `json:"project_id"`
	Name        string    `json:"name"`
	BaseURL     string    `json:"base_url"`
	AuthType    string    `json:"auth_type"`
	SecretName  *string   `json:"secret_name,omitempty"`
	Description string    `json:"description"`
	ApiDocs     string    `json:"api_docs"`
	IsEnabled   bool      `json:"is_enabled"`
	CreatedAt   time.Time `json:"created_at"`
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
	rows, err := projectDB.Query(
		`SELECT id, name, base_url, auth_type, secret_name, description, api_docs, is_enabled, created_at
		 FROM of_service_providers ORDER BY name`)
	if err != nil {
		h.deps.serverError(w, "failed to list service providers", err)
		return
	}
	providers := collectRows(rows, func(rows *sql.Rows) (serviceProvider, error) {
		var p serviceProvider
		var secretName sql.NullString
		p.ProjectID = projectID
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.AuthType, &secretName, &p.Description, &p.ApiDocs, &p.IsEnabled, &p.CreatedAt); err != nil {
			return p, err
		}
		if secretName.Valid {
			p.SecretName = &secretName.String
		}
		return p, nil
	})
	if providers == nil {
		providers = []serviceProvider{}
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
	_, err := projectDB.ExecWrite(
		"UPDATE of_service_providers SET is_enabled = NOT is_enabled WHERE id = ?",
		provID,
	)
	if err != nil {
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
	res, err := projectDB.ExecWrite(
		"DELETE FROM of_service_providers WHERE id = ?",
		provID,
	)
	if err != nil {
		h.deps.Logger.Error("failed to delete service provider", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete provider")
		return
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
