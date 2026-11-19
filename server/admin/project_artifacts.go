package admin

import (
	"net/http"

	"openfabrica/db"
	"openfabrica/store"
)

func respondArtifactVersions(w http.ResponseWriter, projectDB *db.ProjectDB, kind string, id int) {
	writeJSON(w, http.StatusOK, store.ListArtifactVersions(projectDB, kind, id))
}

func respondArtifactRestore(w http.ResponseWriter, r *http.Request, projectDB *db.ProjectDB, kind string, id int) {
	var req struct {
		Version int `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	path, err := store.ArtifactPathByID(projectDB, kind, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
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
	newVersion, err := store.ArtifactRestore(projectDB.Writer(), kind, path, version, "admin")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": kind, "restored_from": version, "version": newVersion,
	})
}

func softDeleteArtifact(projectDB *db.ProjectDB, kind string, id int) (int64, error) {
	return store.SoftDeleteArtifact(projectDB, kind, id)
}
