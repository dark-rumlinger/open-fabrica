package admin

import (
	"errors"
	"net/http"

	"openfabrica/store"
)

// ProjectJobsHandler is the operator window onto the of_jobs background queue.
type ProjectJobsHandler struct {
	deps *Deps
}

// List returns recent jobs, newest first.
func (h *ProjectJobsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListJobs(projectDB))
}

// Cancel marks a pending or running job cancelled.
func (h *ProjectJobsHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	jobID, ok := requireIntParam(w, r, "jobID", "invalid job ID")
	if !ok {
		return
	}
	err := store.CancelJob(projectDB, jobID)
	if errors.Is(err, store.ErrJobNotFound) {
		writeError(w, http.StatusNotFound, "job not found or already completed/cancelled")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to cancel job")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": jobID, "status": "cancelled"})
}
