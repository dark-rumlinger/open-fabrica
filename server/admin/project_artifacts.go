package admin

import (
	"database/sql"
	"net/http"
	"time"

	"openfabrica/db"
	"openfabrica/store"
)

// Shared version-history, edit-attribution, and soft-delete mechanics for the
// of_files-backed artifact handlers; each handler (project_components.go /
// project_layouts.go / project_pages.go / project_files.go) keeps only its
// kind-specific List/Get/Create behavior.

// artifactVersion is the read-only audit row returned by every kind's
// version-history endpoint. Prior bodies are not retained (of_file_history holds
// attribution only), so the shape is just "who changed it, and when".
type artifactVersion struct {
	VersionNumber int       `json:"version_number"`
	ChangedBy     string    `json:"changed_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// artifactPath looks up an of_files path by (kind, id) on the read pool.
func artifactPath(projectDB *db.ProjectDB, kind string, id int) (string, error) {
	var path string
	err := projectDB.QueryRow("SELECT path FROM of_files WHERE kind = ? AND id = ?", kind, id).Scan(&path)
	return path, err
}

// respondArtifactVersions writes the last 20 history rows for (kind, id) as a
// JSON array. A missing artifact or query error yields an empty list with 200,
// the behavior every caller already had.
func respondArtifactVersions(w http.ResponseWriter, projectDB *db.ProjectDB, kind string, id int) {
	path, err := artifactPath(projectDB, kind, id)
	if err != nil {
		writeJSON(w, http.StatusOK, []artifactVersion{})
		return
	}
	rows, err := projectDB.Query(
		"SELECT version_number, changed_by, created_at FROM of_file_history WHERE kind = ? AND path = ? ORDER BY version_number DESC LIMIT 20",
		kind, path,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []artifactVersion{})
		return
	}
	versions := collectRows(rows, func(rows *sql.Rows) (artifactVersion, error) {
		var v artifactVersion
		if err := rows.Scan(&v.VersionNumber, &v.ChangedBy, &v.CreatedAt); err != nil {
			return v, err
		}
		return v, nil
	})
	if versions == nil {
		versions = []artifactVersion{}
	}
	writeJSON(w, http.StatusOK, versions)
}

// respondArtifactRestore reverts (kind, id) to a prior snapshotted body and
// writes the outcome as JSON. The request body carries {"version": N}; an
// omitted or non-positive version restores the most recent snapshot preceding
// the live body (an undo, which also recovers a soft-deleted artifact). Shared by
// every kind's restore endpoint.
func respondArtifactRestore(w http.ResponseWriter, r *http.Request, projectDB *db.ProjectDB, kind string, id int) {
	var req struct {
		Version int `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	path, err := artifactPath(projectDB, kind, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	// Platform-managed files are OpenFabrica-owned and re-emitted; restoring an old
	// body would break the guarantee they provide, so refuse as the write paths do.
	if store.IsPlatformManagedArtifact(projectDB.Reader(), kind, path) {
		writeError(w, http.StatusForbidden, "This is an OpenFabrica-managed file ("+path+") and cannot be restored; the platform owns and re-emits it.")
		return
	}
	version := req.Version
	if version <= 0 {
		version = store.ArtifactRestorableVersion(projectDB.Reader(), kind, path)
		if version == 0 {
			writeError(w, http.StatusBadRequest, "no prior snapshot to restore")
			return
		}
	}
	// Route the restore through the single write connection so it serializes with
	// other project writes (ArtifactRestore reads then re-saves via ArtifactSave).
	newVersion, err := store.ArtifactRestore(projectDB.Writer(), kind, path, version, "admin")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": kind, "restored_from": version, "version": newVersion,
	})
}

// recordArtifactEdit appends an attribution-only history row (no body snapshot)
// so the monotonic version counter advances and the edit is credited to the
// admin actor. Used by the body-only editors (components, layouts). Best-effort,
// matching the inline blocks it replaces.
func recordArtifactEdit(writer *sql.DB, kind string, id int, path string) {
	var maxVer int
	_ = writer.QueryRow("SELECT COALESCE(MAX(version_number), 0) FROM of_file_history WHERE kind = ? AND path = ?", kind, path).Scan(&maxVer)
	_, _ = writer.Exec(
		"INSERT INTO of_file_history (file_id, kind, path, version_number, changed_by) VALUES (?, ?, ?, ?, 'admin')",
		id, kind, path, maxVer+1,
	)
}

// softDeleteArtifact marks (kind, id) deleted and returns the rows affected.
// Callers run any kind-specific reference checks first and map a 0-row result to
// a 404.
func softDeleteArtifact(projectDB *db.ProjectDB, kind string, id int) (int64, error) {
	res, err := projectDB.ExecWrite(
		"UPDATE of_files SET is_deleted = 1, deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE kind = ? AND id = ? AND is_deleted = 0",
		kind, id,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
