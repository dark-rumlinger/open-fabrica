package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"openfabrica/store"
)

// ProjectEndpointsHandler handles API endpoint browsing for project detail views.
type ProjectEndpointsHandler struct {
	deps *Deps
}

type updateEndpointRequest struct {
	Methods      []string `json:"methods"`
	RequiresAuth *bool    `json:"requires_auth"`
	PublicRead   *bool    `json:"public_read"`
	RequiredRole *string  `json:"required_role"`
	RateLimit    *int     `json:"rate_limit"`
}

// List returns all API endpoints for a project.
func (h *ProjectEndpointsHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	endpoints, err := store.ListAllProjectEndpoints(projectDB, projectID)
	if err != nil {
		h.deps.serverError(w, "failed to list endpoints", err)
		return
	}
	writeJSON(w, http.StatusOK, endpoints)
}

// Delete removes an API endpoint by ID.
func (h *ProjectEndpointsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}
	err := store.DeleteAPIEndpoint(projectDB, endpointID)
	if errors.Is(err, store.ErrEndpointNotFound) {
		writeError(w, http.StatusNotFound, "endpoint not found (only data APIs can be deleted here)")
		return
	}
	if err != nil {
		h.deps.serverError(w, "failed to delete endpoint", err)
		return
	}
	writeStatus(w, "deleted")
}

// Get returns a single API endpoint by ID.
func (h *ProjectEndpointsHandler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}
	e, err := store.GetAPIEndpoint(projectDB, projectID, endpointID)
	if errors.Is(err, store.ErrEndpointNotFound) {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	if err != nil {
		h.deps.serverError(w, "failed to load endpoint", err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// Update modifies an existing API endpoint.
func (h *ProjectEndpointsHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}
	var req updateEndpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var upd store.EndpointUpdate
	hasFields := false
	if len(req.Methods) > 0 {
		valid := map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true}
		for _, m := range req.Methods {
			if !valid[strings.ToUpper(m)] {
				writeError(w, http.StatusBadRequest, "invalid method: "+m)
				return
			}
		}
		b, _ := json.Marshal(req.Methods)
		s := string(b)
		upd.MethodsJSON = &s
		hasFields = true
	}
	if req.RequiresAuth != nil {
		upd.RequiresAuth = req.RequiresAuth
		hasFields = true
	}
	if req.PublicRead != nil {
		upd.PublicRead = req.PublicRead
		hasFields = true
	}
	if req.RequiredRole != nil {
		upd.RequiredRole = req.RequiredRole
		hasFields = true
	}
	if req.RateLimit != nil {
		upd.RateLimit = req.RateLimit
		hasFields = true
	}
	if !hasFields {
		writeError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	err := store.UpdateAPIEndpoint(projectDB, endpointID, upd)
	if errors.Is(err, store.ErrEndpointNotFound) {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	if err != nil {
		h.deps.serverError(w, "failed to update endpoint", err)
		return
	}
	writeStatus(w, "updated")
}

type createEndpointRequest struct {
	Path         string   `json:"path"`
	TableName    string   `json:"table_name"`
	Methods      []string `json:"methods"`
	RequiresAuth bool     `json:"requires_auth"`
	PublicRead   bool     `json:"public_read"`
	RequiredRole string   `json:"required_role"`
	OwnerColumn  string   `json:"owner_column"`
	RateLimit    int      `json:"rate_limit"`
}

// Create adds a new API endpoint for a project.
func (h *ProjectEndpointsHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	var req createEndpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Path == "" || req.TableName == "" {
		writeError(w, http.StatusBadRequest, "path and table_name are required")
		return
	}
	if req.OwnerColumn != "" && !req.RequiresAuth && !req.PublicRead {
		writeError(w, http.StatusBadRequest, "owner_column requires requires_auth=true (or set public_read=true for public, unscoped reads)")
		return
	}

	methods := req.Methods
	if len(methods) == 0 {
		methods = []string{"GET", "POST", "PUT", "DELETE"}
	}
	methodsJSON, _ := json.Marshal(methods)
	rateLimit := req.RateLimit
	if rateLimit <= 0 {
		rateLimit = 60
	}

	err := store.CreateAPIEndpoint(projectDB, req.Path, req.TableName, string(methodsJSON), req.RequiresAuth, req.PublicRead, req.RequiredRole, req.OwnerColumn, rateLimit)
	if errors.Is(err, store.ErrTableNotFound) {
		writeError(w, http.StatusBadRequest, "table '"+req.TableName+"' does not exist")
		return
	}
	if errors.Is(err, store.ErrEndpointDuplicate) {
		writeError(w, http.StatusConflict, "endpoint path '"+req.Path+"' already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create endpoint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": req.Path, "created": true})
}
