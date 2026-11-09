package admin

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// ProjectComponentsHandler handles component listing/viewing/deletion for admin.
// Components are HTML fragments stored in of_files (kind=component) and
// resolved at render time by {{include:name}} placeholders inside pages and
// layouts. Shape parallels ProjectLayoutsHandler deliberately, the two kinds
// share a table schema; only their role in the render pipeline differs.
type ProjectComponentsHandler struct {
	deps *Deps
}

type componentEntry struct {
	ID           int       `json:"id"`
	Name         string    `json:"name"`
	Body         string    `json:"body"`
	Description  string    `json:"description"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	VersionCount int       `json:"version_count"`
}

// List returns all components for a project.
func (h *ProjectComponentsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		`SELECT id, path, COALESCE(body, '') AS body,
		        COALESCE(json_extract(metadata, '$.description'), '') AS description,
		        created_at, updated_at,
		        (SELECT COUNT(*) FROM of_file_history WHERE kind = 'component' AND path = of_files.path) AS version_count
		 FROM of_files WHERE kind = 'component' AND is_deleted = 0 ORDER BY path`,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []componentEntry{})
		return
	}
	components := collectRows(rows, func(rows *sql.Rows) (componentEntry, error) {
		var c componentEntry
		if err := rows.Scan(&c.ID, &c.Name, &c.Body, &c.Description, &c.CreatedAt, &c.UpdatedAt, &c.VersionCount); err != nil {
			return c, err
		}
		return c, nil
	})
	if components == nil {
		components = []componentEntry{}
	}

	writeJSON(w, http.StatusOK, components)
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

	var c componentEntry
	err := projectDB.QueryRow(
		`SELECT id, path, COALESCE(body, '') AS body,
		        COALESCE(json_extract(metadata, '$.description'), '') AS description,
		        created_at, updated_at
		 FROM of_files WHERE kind = 'component' AND id = ? AND is_deleted = 0`,
		componentID,
	).Scan(&c.ID, &c.Name, &c.Body, &c.Description, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}

	writeJSON(w, http.StatusOK, c)
}

// Delete removes a component by ID, unless pages or layouts reference it
// via {{include:name}}. Deleting a used component would break the project
// silently at render time, we'd rather block here.
func (h *ProjectComponentsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	componentID, ok := requireIntParam(w, r, "componentID", "invalid component ID")
	if !ok {
		return
	}

	var name string
	err := projectDB.QueryRow("SELECT path FROM of_files WHERE kind = 'component' AND id = ? AND is_deleted = 0", componentID).Scan(&name)
	if err != nil {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}

	// Scan pages + layouts for {{include:name}} references. A single LIKE
	// check covers both kinds in one pass so the caller gets one
	// consolidated "used by N artifact(s)" error.
	needle := "{{include:" + name + "}}"
	var refCount int
	_ = projectDB.QueryRow(
		`SELECT COUNT(*) FROM of_files
		 WHERE kind IN ('page', 'layout') AND is_deleted = 0 AND body LIKE ?`,
		"%"+needle+"%",
	).Scan(&refCount)
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

	db := projectDB.Writer()

	var name string
	if err := db.QueryRow("SELECT path FROM of_files WHERE kind = 'component' AND id = ? AND is_deleted = 0", componentID).Scan(&name); err != nil {
		writeError(w, http.StatusNotFound, "component not found")
		return
	}

	recordArtifactEdit(db, "component", componentID, name)

	if _, err := db.Exec(
		"UPDATE of_files SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = 'component' AND id = ?",
		*req.Body, componentID,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update component")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"updated": componentID})
}
