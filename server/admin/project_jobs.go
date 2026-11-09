package admin

import (
	"database/sql"
	"net/http"
	"time"
)

// ProjectJobsHandler is the operator window onto the of_jobs background queue.
// The engine and action runner enqueue one-shot deferred work here via the
// manage_schedules tool; this surfaces the queue so an operator can see what
// ran (or failed) and cancel pending work without going through chat.
type ProjectJobsHandler struct {
	deps *Deps
}

// List returns recent jobs, newest first. Capped so a busy queue cannot return
// an unbounded payload.
func (h *ProjectJobsHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		`SELECT id, type, status, attempts, max_attempts, error, scheduled_at, completed_at, created_at
		 FROM of_jobs ORDER BY id DESC LIMIT 100`)
	if err != nil {
		// No of_jobs table yet (worker never ran) -> empty list, not an error.
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	jobs := collectRows(rows, func(rows *sql.Rows) (map[string]any, error) {
		var id, attempts, maxAttempts int
		var jobType, status string
		var jobError sql.NullString
		var scheduledAt, completedAt sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&id, &jobType, &status, &attempts, &maxAttempts, &jobError, &scheduledAt, &completedAt, &createdAt); err != nil {
			return nil, err
		}
		job := map[string]any{
			"id": id, "type": jobType, "status": status,
			"attempts": attempts, "max_attempts": maxAttempts, "created_at": createdAt,
		}
		if jobError.Valid && jobError.String != "" {
			job["error"] = jobError.String
		}
		if scheduledAt.Valid {
			job["scheduled_at"] = scheduledAt.Time
		}
		if completedAt.Valid {
			job["completed_at"] = completedAt.Time
		}
		return job, nil
	})
	if jobs == nil {
		jobs = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// Cancel marks a pending or running job cancelled. Mirrors the cancel_job tool
// action: already-completed jobs are left untouched.
func (h *ProjectJobsHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	jobID, ok := requireIntParam(w, r, "jobID", "invalid job ID")
	if !ok {
		return
	}
	res, err := projectDB.ExecWrite(
		"UPDATE of_jobs SET status = 'cancelled' WHERE id = ? AND status IN ('pending', 'running')",
		jobID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to cancel job")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "job not found or already completed/cancelled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": jobID, "status": "cancelled"})
}
