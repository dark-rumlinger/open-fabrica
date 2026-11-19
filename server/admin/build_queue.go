package admin

import (
	"net/http"

	"openfabrica/store"
)

// BuildQueue returns the project's current unified-loop work queue (of_tasks).
func (h *ProjectTasksHandler) BuildQueue(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.LoadBuildQueue(projectDB))
}
