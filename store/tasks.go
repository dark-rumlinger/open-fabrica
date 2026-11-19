package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfabrica/db"
)

// ErrTaskNotFound is returned when a scheduled task row does not exist.
var ErrTaskNotFound = errors.New("task not found")

// ScheduledTask represents a row in of_scheduled_tasks.
type ScheduledTask struct {
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

// ScheduledTaskUpdate describes partial updates to a scheduled task.
type ScheduledTaskUpdate struct {
	Name            *string
	Description     *string
	CronExpression  *string
	IntervalSeconds *int
	Prompt          *string
	IsEnabled       *bool
	NextRun         *time.Time
}

// ListScheduledTasks returns all scheduled tasks for a project ordered newest-first.
func ListScheduledTasks(pdb *db.ProjectDB, projectID int) []ScheduledTask {
	rows, err := pdb.Query(
		`SELECT id, name, description, cron_expression, interval_seconds, prompt, is_enabled, last_run, next_run, created_at, updated_at
		 FROM of_scheduled_tasks ORDER BY created_at DESC`,
	)
	if err != nil {
		return []ScheduledTask{}
	}
	defer rows.Close()

	tasks := []ScheduledTask{}
	for rows.Next() {
		var t ScheduledTask
		t.ProjectID = projectID
		if rows.Scan(&t.ID, &t.Name, &t.Description, &t.CronExpression, &t.IntervalSeconds, &t.Prompt, &t.IsEnabled, &t.LastRun, &t.NextRun, &t.CreatedAt, &t.UpdatedAt) == nil {
			tasks = append(tasks, t)
		}
	}
	_ = rows.Err()
	return tasks
}

// ToggleScheduledTask flips is_enabled atomically and returns the new state.
func ToggleScheduledTask(pdb *db.ProjectDB, taskID int) (bool, error) {
	if _, err := pdb.ExecWrite(
		"UPDATE of_scheduled_tasks SET is_enabled = NOT is_enabled, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		taskID,
	); err != nil {
		return false, err
	}
	var newState bool
	if err := pdb.QueryRow("SELECT is_enabled FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&newState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrTaskNotFound
		}
		return false, err
	}
	return newState, nil
}

// DeleteScheduledTask deletes a scheduled task by ID.
func DeleteScheduledTask(pdb *db.ProjectDB, taskID int) error {
	res, err := pdb.ExecWrite("DELETE FROM of_scheduled_tasks WHERE id = ?", taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// CreateScheduledTask inserts a new scheduled task and returns its ID.
func CreateScheduledTask(pdb *db.ProjectDB, name, description, cronExpr string, intervalSec int, prompt string, nextRun *time.Time) (int64, error) {
	res, err := pdb.ExecWrite(
		"INSERT INTO of_scheduled_tasks (name, description, cron_expression, interval_seconds, prompt, next_run) VALUES (?, ?, ?, ?, ?, ?)",
		name, description, cronExpr, intervalSec, prompt, nextRun,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateScheduledTask applies partial updates to a scheduled task.
func UpdateScheduledTask(pdb *db.ProjectDB, taskID int, u ScheduledTaskUpdate) error {
	setClauses := []string{"updated_at = CURRENT_TIMESTAMP"}
	var values []any
	if u.Name != nil {
		setClauses = append(setClauses, "name = ?")
		values = append(values, *u.Name)
	}
	if u.Description != nil {
		setClauses = append(setClauses, "description = ?")
		values = append(values, *u.Description)
	}
	if u.CronExpression != nil {
		setClauses = append(setClauses, "cron_expression = ?")
		values = append(values, *u.CronExpression)
	}
	if u.IntervalSeconds != nil {
		setClauses = append(setClauses, "interval_seconds = ?")
		values = append(values, *u.IntervalSeconds)
	}
	if u.Prompt != nil {
		setClauses = append(setClauses, "prompt = ?")
		values = append(values, *u.Prompt)
	}
	if u.IsEnabled != nil {
		setClauses = append(setClauses, "is_enabled = ?")
		values = append(values, *u.IsEnabled)
	}
	if u.NextRun != nil {
		setClauses = append(setClauses, "next_run = ?")
		values = append(values, *u.NextRun)
	}
	values = append(values, taskID)
	query := fmt.Sprintf("UPDATE of_scheduled_tasks SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	_, err := pdb.ExecWrite(query, values...)
	return err
}

// TriggerScheduledTaskNow sets next_run to CURRENT_TIMESTAMP and enables the task.
func TriggerScheduledTaskNow(pdb *db.ProjectDB, taskID int) error {
	res, err := pdb.ExecWrite(
		"UPDATE of_scheduled_tasks SET next_run = CURRENT_TIMESTAMP, is_enabled = 1 WHERE id = ?",
		taskID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// ListScheduledTaskRuns returns recent runs for a scheduled task.
func ListScheduledTaskRuns(pdb *db.ProjectDB, taskID int) ([]map[string]any, error) {
	rows, err := pdb.Query(
		`SELECT id, status, error_message, started_at, completed_at
		 FROM of_task_runs WHERE task_id = ? ORDER BY started_at DESC LIMIT 50`,
		taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runs := []map[string]any{}
	for rows.Next() {
		var id int
		var status string
		var errorMsg sql.NullString
		var startedAt time.Time
		var completedAt sql.NullTime
		if rows.Scan(&id, &status, &errorMsg, &startedAt, &completedAt) != nil {
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
		return nil, err
	}
	return runs, nil
}
