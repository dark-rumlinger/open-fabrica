package admin

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"openfabrica/internal/cron"
)

// ProjectTasksHandler handles scheduled task listing for project detail views.
type ProjectTasksHandler struct {
	deps *Deps
}

type scheduledTask struct {
	ID              int        `json:"id"`
	ProjectID       int        `json:"project_id"`
	Name            string     `json:"name"`
	Description     *string    `json:"description"`
	CronExpression  *string    `json:"cron_expression"`
	IntervalSeconds *int       `json:"interval_seconds"`
	Prompt          *string    `json:"prompt"`
	IsEnabled       bool       `json:"is_enabled"`
	LastRun         *time.Time `json:"last_run"`
	NextRun         *time.Time `json:"next_run"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// List returns all scheduled tasks for a project.
func (h *ProjectTasksHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	rows, err := projectDB.Query(
		`SELECT id, name, description, cron_expression, interval_seconds, prompt, is_enabled, last_run, next_run, created_at, updated_at
		 FROM of_scheduled_tasks ORDER BY created_at DESC`,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []scheduledTask{})
		return
	}

	tasks := collectRows(rows, func(rows *sql.Rows) (scheduledTask, error) {
		var t scheduledTask
		t.ProjectID = projectID
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.CronExpression, &t.IntervalSeconds, &t.Prompt, &t.IsEnabled, &t.LastRun, &t.NextRun, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return t, err
		}
		return t, nil
	})

	if tasks == nil {
		tasks = []scheduledTask{}
	}

	writeJSON(w, http.StatusOK, tasks)
}

// Toggle enables or disables a scheduled task.
func (h *ProjectTasksHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	taskID, ok := requireIntParam(w, r, "taskID", "invalid task ID")
	if !ok {
		return
	}

	// Flip atomically (matches the webhook/trigger toggles) so concurrent
	// requests can't race on a read-then-write, then read back the new state.
	if _, err := projectDB.ExecWrite(
		"UPDATE of_scheduled_tasks SET is_enabled = NOT is_enabled, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		taskID,
	); err != nil {
		h.deps.serverError(w, "failed to toggle task", err)
		return
	}

	var newState bool
	if err := projectDB.QueryRow("SELECT is_enabled FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&newState); err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"is_enabled": newState})
}

// Delete removes a scheduled task.
func (h *ProjectTasksHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	taskID, ok := requireIntParam(w, r, "taskID", "invalid task ID")
	if !ok {
		return
	}

	result, err := projectDB.ExecWrite("DELETE FROM of_scheduled_tasks WHERE id = ?", taskID)
	if err != nil {
		h.deps.serverError(w, "failed to delete task", err)
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	writeStatus(w, "deleted")
}

// Create adds a new scheduled task for a project.
func (h *ProjectTasksHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var req struct {
		Name            string `json:"name"`
		Description     string `json:"description"`
		CronExpression  string `json:"cron_expression"`
		IntervalSeconds int    `json:"interval_seconds"`
		Prompt          string `json:"prompt"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "name and prompt are required")
		return
	}
	if req.CronExpression == "" && req.IntervalSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "a cron expression or a positive interval is required")
		return
	}
	// Validate cron if provided
	if req.CronExpression != "" {
		if err := cron.Validate(req.CronExpression); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
			return
		}
	}
	var nextRun *time.Time
	nr, ok, err := cron.NextRun(req.CronExpression, req.IntervalSeconds, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "cron expression has no upcoming run: "+err.Error())
		return
	}
	if ok {
		nextRun = &nr
	}
	result, err := projectDB.ExecWrite(
		"INSERT INTO of_scheduled_tasks (name, description, cron_expression, interval_seconds, prompt, next_run) VALUES (?, ?, ?, ?, ?, ?)",
		req.Name, req.Description, req.CronExpression, req.IntervalSeconds, req.Prompt, nextRun,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create task")
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name})
}

// Update modifies an existing scheduled task.
func (h *ProjectTasksHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	taskID, ok := requireIntParam(w, r, "taskID", "invalid task ID")
	if !ok {
		return
	}

	var req struct {
		Name            *string `json:"name"`
		Description     *string `json:"description"`
		CronExpression  *string `json:"cron_expression"`
		IntervalSeconds *int    `json:"interval_seconds"`
		Prompt          *string `json:"prompt"`
		IsEnabled       *bool   `json:"is_enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// When both schedule fields are supplied (the edit form always sends both),
	// at least one must define a schedule, otherwise the task would never run.
	if req.CronExpression != nil && req.IntervalSeconds != nil &&
		*req.CronExpression == "" && *req.IntervalSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "a cron expression or a positive interval is required")
		return
	}
	setClauses := []string{"updated_at = CURRENT_TIMESTAMP"}
	var values []any
	scheduleChanged := false
	var newCron string
	var newInterval int
	if req.Name != nil {
		setClauses = append(setClauses, "name = ?")
		values = append(values, *req.Name)
	}
	if req.Description != nil {
		setClauses = append(setClauses, "description = ?")
		values = append(values, *req.Description)
	}
	if req.CronExpression != nil {
		if *req.CronExpression != "" {
			if err := cron.Validate(*req.CronExpression); err != nil {
				writeError(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
				return
			}
		}
		setClauses = append(setClauses, "cron_expression = ?")
		values = append(values, *req.CronExpression)
		newCron = *req.CronExpression
		scheduleChanged = true
	}
	if req.IntervalSeconds != nil {
		setClauses = append(setClauses, "interval_seconds = ?")
		values = append(values, *req.IntervalSeconds)
		newInterval = *req.IntervalSeconds
		scheduleChanged = true
	}
	if req.Prompt != nil {
		setClauses = append(setClauses, "prompt = ?")
		values = append(values, *req.Prompt)
	}
	if req.IsEnabled != nil {
		setClauses = append(setClauses, "is_enabled = ?")
		values = append(values, *req.IsEnabled)
	}
	values = append(values, taskID)
	query := fmt.Sprintf("UPDATE of_scheduled_tasks SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	_, err := projectDB.ExecWrite(query, values...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update task")
		return
	}
	if scheduleChanged {
		nextRun, ok, err := cron.NextRun(newCron, newInterval, time.Now())
		if err != nil {
			// Validated above, so this is an impossible schedule (e.g. Feb 30):
			// leave next_run untouched so it does not fire with a garbage time.
			slog.Warn("scheduled task has no upcoming run; leaving next_run unchanged", "task_id", taskID, "cron", newCron, "error", err)
		}
		if ok {
			if _, err := projectDB.ExecWrite("UPDATE of_scheduled_tasks SET next_run = ? WHERE id = ?", nextRun, taskID); err != nil {
				// The core fields already saved, but the schedule recompute did not
				// persist: report failure rather than claim success while the
				// scheduler keeps firing on the old next_run.
				writeError(w, http.StatusInternalServerError, "failed to update task schedule")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": taskID, "status": "updated"})
}

// RunNow triggers immediate execution of a scheduled task by setting next_run to now.
func (h *ProjectTasksHandler) RunNow(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	taskID, ok := requireIntParam(w, r, "taskID", "invalid task ID")
	if !ok {
		return
	}

	res, err := projectDB.ExecWrite(
		"UPDATE of_scheduled_tasks SET next_run = CURRENT_TIMESTAMP, is_enabled = 1 WHERE id = ?",
		taskID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to trigger task")
		return
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"triggered": taskID})
}

// TaskRuns returns recent runs for a scheduled task.
func (h *ProjectTasksHandler) TaskRuns(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	taskID, ok := requireIntParam(w, r, "taskID", "invalid task ID")
	if !ok {
		return
	}
	rows, err := projectDB.Query(
		`SELECT id, status, error_message, started_at, completed_at
		 FROM of_task_runs WHERE task_id = ? ORDER BY started_at DESC LIMIT 50`, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list task runs")
		return
	}
	defer rows.Close()
	var runs []map[string]any
	for rows.Next() {
		var id int
		var status string
		var errorMsg sql.NullString
		var startedAt time.Time
		var completedAt sql.NullTime
		if err := rows.Scan(&id, &status, &errorMsg, &startedAt, &completedAt); err != nil {
			continue
		}
		run := map[string]any{"id": id, "status": status, "started_at": startedAt}
		if errorMsg.Valid {
			run["error_message"] = errorMsg.String
		}
		if completedAt.Valid {
			run["completed_at"] = completedAt.Time
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list task runs")
		return
	}
	if runs == nil {
		runs = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, runs)
}
