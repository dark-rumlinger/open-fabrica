package admin

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

// ProjectLayoutsHandler handles layout listing/viewing/deletion for admin. All
// storage goes through of_files (kind=layout); IDs and version history
// come from of_files / of_file_history.
type ProjectLayoutsHandler struct {
	deps *Deps
}

type layoutEntry struct {
	ID           int       `json:"id"`
	Name         string    `json:"name"`
	Template     string    `json:"template"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	VersionCount int       `json:"version_count"`
}

// List returns all layouts for a project.
func (h *ProjectLayoutsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		`SELECT id, path, COALESCE(body, '') AS template, created_at, updated_at,
		        (SELECT COUNT(*) FROM of_file_history WHERE kind = 'layout' AND path = of_files.path) AS version_count
		 FROM of_files WHERE kind = 'layout' AND is_deleted = 0 ORDER BY path`,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []layoutEntry{})
		return
	}
	layouts := collectRows(rows, func(rows *sql.Rows) (layoutEntry, error) {
		var l layoutEntry
		if err := rows.Scan(&l.ID, &l.Name, &l.Template, &l.CreatedAt, &l.UpdatedAt, &l.VersionCount); err != nil {
			return l, err
		}
		return l, nil
	})
	if layouts == nil {
		layouts = []layoutEntry{}
	}

	writeJSON(w, http.StatusOK, layouts)
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

	var l layoutEntry
	err := projectDB.QueryRow(
		"SELECT id, path, COALESCE(body, '') AS template, created_at, updated_at FROM of_files WHERE kind = 'layout' AND id = ? AND is_deleted = 0",
		layoutID,
	).Scan(&l.ID, &l.Name, &l.Template, &l.CreatedAt, &l.UpdatedAt)
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

	var name string
	err := projectDB.QueryRow("SELECT path FROM of_files WHERE kind = 'layout' AND id = ? AND is_deleted = 0", layoutID).Scan(&name)
	if err != nil {
		writeError(w, http.StatusNotFound, "layout not found")
		return
	}

	// Check if pages reference this layout.
	var pageCount int
	_ = projectDB.QueryRow(
		"SELECT COUNT(*) FROM of_files WHERE kind = 'page' AND json_extract(metadata, '$.layout') = ? AND is_deleted = 0",
		name,
	).Scan(&pageCount)
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

	db := projectDB.Writer()

	var name string
	if err := db.QueryRow("SELECT path FROM of_files WHERE kind = 'layout' AND id = ? AND is_deleted = 0", layoutID).Scan(&name); err != nil {
		writeError(w, http.StatusNotFound, "layout not found")
		return
	}

	recordArtifactEdit(db, "layout", layoutID, name)

	if _, err := db.Exec(
		"UPDATE of_files SET body = ?, updated_at = CURRENT_TIMESTAMP WHERE kind = 'layout' AND id = ?",
		*req.Template, layoutID,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update layout")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"updated": layoutID})
}
