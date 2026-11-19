package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"openfabrica/internal/cron"
	"openfabrica/store"
)

// ProjectTasksHandler handles scheduled task listing for project detail views.
type ProjectTasksHandler struct {
	deps *Deps
}

// List returns all scheduled tasks for a project.
func (h *ProjectTasksHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	writeJSON(w, http.StatusOK, store.ListScheduledTasks(projectDB, projectID))
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

	newState, err := store.ToggleScheduledTask(projectDB, taskID)
	if errors.Is(err, store.ErrTaskNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		h.deps.serverError(w, "failed to toggle task", err)
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

	err := store.DeleteScheduledTask(projectDB, taskID)
	if errors.Is(err, store.ErrTaskNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		h.deps.serverError(w, "failed to delete task", err)
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
	id, err := store.CreateScheduledTask(projectDB, req.Name, req.Description, req.CronExpression, req.IntervalSeconds, req.Prompt, nextRun)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create task")
		return
	}
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
	if req.CronExpression != nil && req.IntervalSeconds != nil &&
		*req.CronExpression == "" && *req.IntervalSeconds <= 0 {
		writeError(w, http.StatusBadRequest, "a cron expression or a positive interval is required")
		return
	}
	if req.CronExpression != nil && *req.CronExpression != "" {
		if err := cron.Validate(*req.CronExpression); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
			return
		}
	}

	var nextRun *time.Time
	if req.CronExpression != nil || req.IntervalSeconds != nil {
		var newCron string
		if req.CronExpression != nil {
			newCron = *req.CronExpression
		}
		var newInterval int
		if req.IntervalSeconds != nil {
			newInterval = *req.IntervalSeconds
		}
		nr, hasNext, err := cron.NextRun(newCron, newInterval, time.Now())
		if err != nil {
			slog.Warn("scheduled task has no upcoming run; leaving next_run unchanged", "task_id", taskID, "cron", newCron, "error", err)
		} else if hasNext {
			nextRun = &nr
		}
	}

	if err := store.UpdateScheduledTask(projectDB, taskID, store.ScheduledTaskUpdate{
		Name:            req.Name,
		Description:     req.Description,
		CronExpression:  req.CronExpression,
		IntervalSeconds: req.IntervalSeconds,
		Prompt:          req.Prompt,
		IsEnabled:       req.IsEnabled,
		NextRun:         nextRun,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update task")
		return
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

	err := store.TriggerScheduledTaskNow(projectDB, taskID)
	if errors.Is(err, store.ErrTaskNotFound) {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to trigger task")
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

	runs, err := store.ListScheduledTaskRuns(projectDB, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list task runs")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}
