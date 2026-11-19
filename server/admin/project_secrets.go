package admin

import (
	"errors"
	"net/http"

	"openfabrica/store"
)

// ProjectSecretsHandler handles secret listing/deletion for admin.
type ProjectSecretsHandler struct {
	deps *Deps
}

// List returns all secrets for a project (names only, never values).
func (h *ProjectSecretsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListSecrets(projectDB))
}

// Delete removes a secret by ID.
func (h *ProjectSecretsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	secretID, ok := requireIntParam(w, r, "secretID", "invalid secret ID")
	if !ok {
		return
	}
	err := store.DeleteSecret(projectDB, secretID)
	if errors.Is(err, store.ErrSecretNotFound) {
		writeError(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": secretID})
}

type createSecretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Create stores a new encrypted secret.
func (h *ProjectSecretsHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	var req createSecretRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.Value == "" {
		writeError(w, http.StatusBadRequest, "name and value are required")
		return
	}
	encrypted, err := h.deps.Encryptor.Encrypt(req.Value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt secret")
		return
	}
	if err := store.UpsertEncryptedSecret(projectDB, req.Name, encrypted); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": req.Name})
}

type updateSecretRequest struct {
	Value string `json:"value"`
}

// Update replaces the value of an existing secret, addressed by ID.
func (h *ProjectSecretsHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	secretID, ok := requireIntParam(w, r, "secretID", "invalid secret ID")
	if !ok {
		return
	}
	var req updateSecretRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, "value is required")
		return
	}
	encrypted, err := h.deps.Encryptor.Encrypt(req.Value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encrypt secret")
		return
	}
	err = store.UpdateSecretValue(projectDB, secretID, encrypted)
	if errors.Is(err, store.ErrSecretNotFound) {
		writeError(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": secretID})
}
