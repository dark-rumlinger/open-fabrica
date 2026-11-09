package admin

import (
	"database/sql"
	"net/http"
	"time"
)

// ProjectSecretsHandler handles secret listing/deletion for admin.
type ProjectSecretsHandler struct {
	deps *Deps
}

type secretEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	// HasValue is false for a secret whose value is empty, which is how an imported
	// project arrives: names are kept, values are blanked. The value itself never
	// leaves the server.
	HasValue  bool      `json:"has_value"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// List returns all secrets for a project (names only, never values).
func (h *ProjectSecretsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		"SELECT id, name, value_encrypted <> '', created_at, updated_at FROM of_secrets ORDER BY name",
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []secretEntry{})
		return
	}
	secrets := collectRows(rows, func(rows *sql.Rows) (secretEntry, error) {
		var s secretEntry
		if err := rows.Scan(&s.ID, &s.Name, &s.HasValue, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return s, err
		}
		return s, nil
	})

	if secrets == nil {
		secrets = []secretEntry{}
	}

	writeJSON(w, http.StatusOK, secrets)
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

	res, err := projectDB.ExecWrite(
		"DELETE FROM of_secrets WHERE id = ?",
		secretID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete secret")
		return
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "secret not found")
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

	_, err = projectDB.ExecWrite(
		`INSERT INTO of_secrets (name, value_encrypted, updated_at)
		 VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
		req.Name, encrypted,
	)
	if err != nil {
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

	res, err := projectDB.ExecWrite(
		"UPDATE of_secrets SET value_encrypted = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		encrypted, secretID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update secret")
		return
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "secret not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"updated": secretID})
}
