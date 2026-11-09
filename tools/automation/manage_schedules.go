package automation

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/cron"
	"openfabrica/tools"
)

// manage_schedules registers two related subsystems in one tool: the one-shot
// background-job queue (of_jobs) and the cron/interval scheduler (of_scheduled_tasks).
// Grouping them keeps the LLM catalogue small - both are time-flavoured automation.

func init() {
	tools.Define(tools.Tool{
		Name:     "manage_schedules",
		Category: "automation",
		// list actions return full rows; the 3000-byte default would cut them mid-JSON.
		MaxResult: 8000,
		OneLiner:  "Manage the background job queue and cron/interval scheduled tasks.",
		WhenToUse: `Use jobs (enqueue_job/job_status/list_jobs/cancel_job) for one-shot deferred work. Use
schedules (create_schedule/list_schedules/update_schedule/delete_schedule/run_schedule_now)
for recurring work - cron expressions or interval_seconds.

A schedule runs ONE of two ways and the difference is expensive: native_action runs
directly with no LLM and any cadence, prompt wakes the FULL agent every run. Default to
native_action; the prompt arg says when a prompt is the right call.`,
		Actions: []tools.Action{
			&JobEnqueue{},
			&JobStatus{},
			&JobList{},
			&JobCancel{},
			&ScheduleCreate{},
			&ScheduleList{},
			&ScheduleUpdate{},
			&ScheduleDelete{},
			&ScheduleRunNow{},
		},
		Hints: func(action string, _ map[string]any, r *tools.Result) string {
			switch action {
			case "create_schedule":
				// Only what this result adds: the exact cron grammar and a filled-in
				// native_action. Which mode to choose is the prompt/native_action args'.
				return "Cron: 5 numeric fields 'minute hour day_of_month month day_of_week' (ranges 0-59 0-23 1-31 1-12 0-6); supports *, ranges (1-5), steps (*/5), lists (1,3,5). Or set interval_seconds instead. native_action is a JSON-encoded STRING (not an object), e.g. {\"type\":\"count_rows\",\"table\":\"x\"} or {\"type\":\"run_function\",\"endpoint_path\":\"sync-rates\"} to run a manage_function handler on a schedule."
			case "run_schedule_now":
				if r == nil || !r.Success {
					return ""
				}
				return "Forces the task's next run to the next scheduler tick (~30s); only fires enabled tasks. list_schedules then shows its last_run and next_run."
			}
			return ""
		},
	})
}

// Job queue actions (one-shot deferred work, backed by of_jobs)

// JobEnqueue queues a background job. The job worker picks up "pending" rows
// and dispatches by type; scheduled_at lets you defer execution.
type JobEnqueue struct {
	Type        string `arg:"type,required" desc:"Job type." enum:"send_email,http_request,run_sql,llm_prompt"`
	Payload     string `arg:"payload"       desc:"JSON string with the job's parameters, by type. send_email: {to, subject, body_html|body_text|template_name, template_vars}. http_request: {url, method (GET unless set), headers, body}. run_sql: {sql}, one read-only SELECT (use a trigger's run_sql action to write). llm_prompt: {prompt, system, max_tokens (512 unless set)}." default:"{}"`
	ScheduledAt string `arg:"scheduled_at"  desc:"ISO datetime for delayed execution."`
	MaxAttempts int    `arg:"max_attempts"  desc:"Max retry attempts (1-10; out-of-range is rejected)." default:"3" min:"1" max:"10"`
}

func (a *JobEnqueue) ActionName() string { return "enqueue_job" }
func (a *JobEnqueue) Doc() string        { return "Queue a background job (one-shot, may be delayed)." }
func (a *JobEnqueue) Example() any {
	return &JobEnqueue{Type: "send_email", Payload: `{"to":"a@b.com","subject":"Your report","body_text":"It is ready."}`}
}
func (a *JobEnqueue) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	// The worker decodes payload when the job runs; a malformed one failed there,
	// silently and after the build moved on. Reject it now with the parse error.
	if strings.TrimSpace(a.Payload) != "" {
		var probe map[string]any
		if err := json.Unmarshal([]byte(a.Payload), &probe); err != nil {
			return tools.FailCode(tools.CodeBadArg, "payload must be a JSON object string: %v", err), nil
		}
	}
	maxAttempts := a.MaxAttempts
	var result sql.Result
	var err error
	if a.ScheduledAt != "" {
		result, err = ctx.DB.Exec(
			"INSERT INTO of_jobs (type, payload, max_attempts, scheduled_at) VALUES (?, ?, ?, ?)",
			a.Type, a.Payload, maxAttempts, a.ScheduledAt,
		)
	} else {
		result, err = ctx.DB.Exec(
			"INSERT INTO of_jobs (type, payload, max_attempts) VALUES (?, ?, ?)",
			a.Type, a.Payload, maxAttempts,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("enqueuing job: %w", err)
	}
	id, _ := result.LastInsertId()
	return tools.Ok(map[string]any{
		"id": id, "type": a.Type, "status": "pending", "max_attempts": maxAttempts,
	}), nil
}

// JobStatus returns one job's row with its current execution state.
type JobStatus struct {
	ID int `arg:"id,required" desc:"Job id."`
}

func (a *JobStatus) Doc() string    { return "Fetch one job's current status and result." }
func (a *JobStatus) ReadOnly() bool { return true }
func (a *JobStatus) Example() any   { return &JobStatus{ID: 1} }
func (a *JobStatus) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	var jobType, status, payload, jobResult, jobError string
	var attempts, maxAttempts int
	var scheduledAt, startedAt, completedAt, createdAt sql.NullString
	err := ctx.DB.QueryRow(
		`SELECT type, payload, status, attempts, max_attempts, result, error,
		        scheduled_at, started_at, completed_at, created_at
		 FROM of_jobs WHERE id = ?`, a.ID,
	).Scan(&jobType, &payload, &status, &attempts, &maxAttempts, &jobResult, &jobError,
		&scheduledAt, &startedAt, &completedAt, &createdAt)
	if err != nil {
		return tools.Fail("job %d not found", a.ID), nil
	}
	data := map[string]any{
		"id": a.ID, "type": jobType, "status": status,
		"attempts": attempts, "max_attempts": maxAttempts,
	}
	if jobResult != "" {
		data["result"] = jobResult
	}
	if jobError != "" {
		data["error"] = jobError
	}
	if scheduledAt.Valid {
		data["scheduled_at"] = scheduledAt.String
	}
	if startedAt.Valid {
		data["started_at"] = startedAt.String
	}
	if completedAt.Valid {
		data["completed_at"] = completedAt.String
	}
	return tools.Ok(data), nil
}

// JobList paginates the jobs table with optional type/status filters.
type JobList struct {
	Status string `arg:"status" desc:"Filter by status (pending, running, completed, failed, cancelled)."`
	Type   string `arg:"type"   desc:"Filter by job type."`
	Limit  int    `arg:"limit"  desc:"Max rows returned (maximum 200)." default:"50" max:"200"`
}

func (a *JobList) ActionName() string { return "list_jobs" }
func (a *JobList) Doc() string        { return "List jobs, optionally filtered by type or status." }
func (a *JobList) ReadOnly() bool     { return true }
func (a *JobList) Example() any {
	return &JobList{Status: "pending"}
}
func (a *JobList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	limit := a.Limit
	query := "SELECT id, type, status, attempts, max_attempts, error, created_at FROM of_jobs"
	var conditions []string
	var queryArgs []any
	if a.Status != "" {
		conditions = append(conditions, "status = ?")
		queryArgs = append(queryArgs, a.Status)
	}
	if a.Type != "" {
		conditions = append(conditions, "type = ?")
		queryArgs = append(queryArgs, a.Type)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	queryArgs = append(queryArgs, limit)
	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	defer rows.Close()
	var jobs []map[string]any
	for rows.Next() {
		var id, attempts, maxAttempts int
		var jobType, status, jobError, createdAt string
		if rows.Scan(&id, &jobType, &status, &attempts, &maxAttempts, &jobError, &createdAt) != nil {
			continue
		}
		job := map[string]any{
			"id": id, "type": jobType, "status": status,
			"attempts": attempts, "max_attempts": maxAttempts, "created_at": createdAt,
		}
		if jobError != "" {
			job["error"] = jobError
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	return tools.Ok(jobs), nil
}

// JobCancel marks a pending/running job cancelled. Idempotent only insofar
// as already-completed jobs return a "not found / already done" error.
type JobCancel struct {
	ID int `arg:"id,required" desc:"Job id."`
}

func (a *JobCancel) ActionName() string { return "cancel_job" }
func (a *JobCancel) Doc() string        { return "Cancel a pending or running job." }
func (a *JobCancel) Example() any       { return &JobCancel{ID: 1} }
func (a *JobCancel) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	result, err := ctx.DB.Exec(
		"UPDATE of_jobs SET status = 'cancelled' WHERE id = ? AND status IN ('pending', 'running')",
		a.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("cancelling job: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return tools.Fail("job %d not found or already completed/cancelled", a.ID), nil
	}
	return tools.Ok(map[string]any{"id": a.ID, "status": "cancelled"}), nil
}

// Scheduled-task actions (recurring work, backed by of_scheduled_tasks)

// ScheduleCreate registers a cron- or interval-driven recurring task. Either
// prompt (LLM-driven) or native_action (direct DB/event op) is required.
type ScheduleCreate struct {
	Name            string `arg:"name,required"   desc:"Task name."`
	Description     string `arg:"description"     desc:"Free-form description."`
	CronExpression  string `arg:"cron_expression" desc:"5-field cron expression in the server's local time (e.g. '0 */6 * * *')."`
	IntervalSeconds int    `arg:"interval_seconds" desc:"Alternative to cron: run every N seconds."`
	Prompt          string `arg:"prompt"          desc:"Prompt run when the task fires. Wakes the FULL agent every run (LLM cost), so it must be hourly or slower and is for periodic reasoning only (digests, reminders)."`
	NativeAction    string `arg:"native_action"   desc:"JSON-encoded STRING of an action that runs WITHOUT the LLM (cheap, any cadence). type: delete_stale {table, column (default created_at), age \"N days\" (units up to years, no weeks)} | count_rows {table} | truncate {table} | sql {sql, params: [...] bound to ? placeholders; SELECT/INSERT/UPDATE/DELETE, one statement} | trigger_event {event_type, payload?} | run_function {endpoint_path}. run_function invokes a manage_function handler on a schedule (config {\"type\":\"run_function\",\"endpoint_path\":\"<function path>\"}) - use it for recurring custom logic (nightly 'fetch exchange rates and write them', 'poll an API and enqueue work'). Prefer a native_action over a prompt for any non-reasoning upkeep."`
}

func (a *ScheduleCreate) ActionName() string { return "create_schedule" }
func (a *ScheduleCreate) Doc() string {
	// Which of the two modes to pick, and the heartbeat ban, are the prompt arg's.
	return "Create a cron- or interval-driven recurring task, driven by either a native_action or a prompt."
}
func (a *ScheduleCreate) Example() any {
	return &ScheduleCreate{
		Name: "prune sessions", CronExpression: "0 3 * * *",
		NativeAction: `{"type":"delete_stale","table":"sessions","age":"1 day"}`,
	}
}
func (a *ScheduleCreate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	if a.Prompt == "" && a.NativeAction == "" {
		return tools.Fail("either prompt or native_action is required"), nil
	}
	if a.Prompt != "" && a.NativeAction != "" {
		return tools.FailCode(tools.CodeBadArg, "pass prompt OR native_action, not both - a schedule runs one of them"), nil
	}
	if a.CronExpression != "" && a.IntervalSeconds > 0 {
		return tools.FailCode(tools.CodeBadArg, "pass cron_expression OR interval_seconds, not both"), nil
	}
	if a.CronExpression == "" && a.IntervalSeconds <= 0 {
		return tools.FailCode(tools.CodeMissingArg, "a recurring schedule needs cron_expression or interval_seconds; for one delayed action use enqueue_job with scheduled_at"), nil
	}
	if err := validateNativeAction(a.NativeAction); err != nil {
		return tools.Fail("invalid native_action: %s", err.Error()), nil
	}
	if a.CronExpression != "" {
		if err := cron.Validate(a.CronExpression); err != nil {
			return tools.Fail("invalid cron expression: %s", err.Error()), nil
		}
	}
	now := time.Now()
	if reason := guardLLMCadence(a.Prompt, a.NativeAction, a.CronExpression, a.IntervalSeconds, now); reason != "" {
		return tools.Fail("schedule rejected: %s", reason), nil
	}
	var nextRun *time.Time
	nr, ok, err := cron.NextRun(a.CronExpression, a.IntervalSeconds, now)
	if err != nil {
		return tools.Fail("cron expression has no upcoming run: %s", err.Error()), nil
	}
	if ok {
		nextRun = &nr
	}
	result, err := ctx.DB.Exec(
		"INSERT INTO of_scheduled_tasks (name, description, cron_expression, interval_seconds, prompt, native_action, next_run) VALUES (?, ?, ?, ?, ?, ?, ?)",
		a.Name, a.Description, a.CronExpression, a.IntervalSeconds, a.Prompt, a.NativeAction, nextRun,
	)
	if err != nil {
		return nil, fmt.Errorf("creating task: %w", err)
	}
	id, _ := result.LastInsertId()
	data := map[string]any{"id": id, "name": a.Name}
	if a.NativeAction != "" {
		data["native_action"] = true
	}
	if nextRun != nil {
		data["next_run"] = *nextRun
	}
	return tools.Ok(data), nil
}

// ScheduleList returns every scheduled task with its cron/interval and most
// recent run metadata.
type ScheduleList struct{}

func (a *ScheduleList) ActionName() string { return "list_schedules" }
func (a *ScheduleList) Doc() string        { return "List every scheduled task." }
func (a *ScheduleList) ReadOnly() bool     { return true }
func (a *ScheduleList) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT id, name, description, cron_expression, interval_seconds, prompt, native_action, is_enabled, last_run, next_run, created_at FROM of_scheduled_tasks ORDER BY name",
	)
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	defer rows.Close()
	var tasks []map[string]any
	for rows.Next() {
		var id int
		var name string
		var description, cronExpr, prompt, nativeAction sql.NullString
		var intervalSec sql.NullInt64
		var isEnabled bool
		var lastRun, nextRun sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &description, &cronExpr, &intervalSec, &prompt, &nativeAction, &isEnabled, &lastRun, &nextRun, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning task: %w", err)
		}
		task := map[string]any{
			"id": id, "name": name, "is_enabled": isEnabled, "created_at": createdAt,
		}
		if description.Valid {
			task["description"] = description.String
		}
		if cronExpr.Valid {
			task["cron_expression"] = cronExpr.String
		}
		if intervalSec.Valid {
			task["interval_seconds"] = intervalSec.Int64
		}
		if prompt.Valid && prompt.String != "" {
			task["prompt"] = prompt.String
		}
		if nativeAction.Valid && nativeAction.String != "" {
			task["native_action"] = nativeAction.String
		}
		if lastRun.Valid {
			task["last_run"] = lastRun.Time
		}
		if nextRun.Valid {
			task["next_run"] = nextRun.Time
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	return tools.Ok(tasks), nil
}

// ScheduleUpdate patches one or more fields of an existing schedule and
// recomputes next_run when the trigger pattern changed.
type ScheduleUpdate struct {
	ID              int64  `arg:"id,required"     desc:"Task id."`
	Name            string `arg:"name"            desc:"New task name."`
	Description     string `arg:"description"     desc:"New description."`
	CronExpression  string `arg:"cron_expression" desc:"Switch to this cron expression (clears the interval)."`
	IntervalSeconds *int   `arg:"interval_seconds" desc:"Switch to running every N seconds (clears the cron expression)."`
	Prompt          string `arg:"prompt"          desc:"New LLM prompt."`
	NativeAction    string `arg:"native_action"   desc:"New native action JSON."`
	IsEnabled       *bool  `arg:"is_enabled"      desc:"Enable or disable."`
}

func (a *ScheduleUpdate) ActionName() string { return "update_schedule" }
func (a *ScheduleUpdate) Doc() string        { return "Update an existing schedule's fields." }
func (a *ScheduleUpdate) Example() any {
	enabled := false
	return &ScheduleUpdate{ID: 1, IsEnabled: &enabled}
}
func (a *ScheduleUpdate) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	setClauses := []string{"updated_at = CURRENT_TIMESTAMP"}
	var values []any

	if a.Name != "" {
		setClauses = append(setClauses, "name = ?")
		values = append(values, a.Name)
	}
	if a.Description != "" {
		setClauses = append(setClauses, "description = ?")
		values = append(values, a.Description)
	}
	var newCronExpr string
	var newIntervalSec int
	var scheduleChanged bool
	if a.CronExpression != "" {
		if err := cron.Validate(a.CronExpression); err != nil {
			return tools.Fail("invalid cron expression: %s", err.Error()), nil
		}
		if a.IntervalSeconds != nil && *a.IntervalSeconds > 0 {
			return tools.FailCode(tools.CodeBadArg, "pass cron_expression OR interval_seconds, not both"), nil
		}
		// Switching to a cron clears the interval, and vice versa: the scheduler
		// prefers a stored cron, so a leftover one would keep firing on its own beat.
		setClauses = append(setClauses, "cron_expression = ?", "interval_seconds = 0")
		values = append(values, a.CronExpression)
		newCronExpr = a.CronExpression
		scheduleChanged = true
	} else if a.IntervalSeconds != nil {
		setClauses = append(setClauses, "interval_seconds = ?", "cron_expression = ''")
		values = append(values, *a.IntervalSeconds)
		newIntervalSec = *a.IntervalSeconds
		scheduleChanged = true
	}
	if a.Prompt != "" {
		setClauses = append(setClauses, "prompt = ?")
		values = append(values, a.Prompt)
	}
	if a.NativeAction != "" {
		if err := validateNativeAction(a.NativeAction); err != nil {
			return tools.Fail("invalid native_action: %s", err.Error()), nil
		}
		setClauses = append(setClauses, "native_action = ?")
		values = append(values, a.NativeAction)
	}
	if a.IsEnabled != nil {
		setClauses = append(setClauses, "is_enabled = ?")
		values = append(values, *a.IsEnabled)
	}

	if scheduleChanged || a.Prompt != "" {
		// Guard the EFFECTIVE cadence so a prompt schedule cannot be sped up below
		// the LLM floor. Merge this update's prompt/native_action with the stored
		// row, so changing only the cron still triggers the check - and adding a
		// prompt to an existing fast schedule (cadence untouched) is checked against
		// the stored cadence.
		if !scheduleChanged {
			var rowCron sql.NullString
			var rowInterval sql.NullInt64
			_ = ctx.DB.QueryRow(
				"SELECT COALESCE(cron_expression,''), COALESCE(interval_seconds,0) FROM of_scheduled_tasks WHERE id = ?",
				a.ID).Scan(&rowCron, &rowInterval)
			newCronExpr, newIntervalSec = rowCron.String, int(rowInterval.Int64)
		}
		effPrompt, effNative := a.Prompt, a.NativeAction
		if effPrompt == "" || effNative == "" {
			var rowPrompt, rowNative sql.NullString
			_ = ctx.DB.QueryRow(
				"SELECT COALESCE(prompt,''), COALESCE(native_action,'') FROM of_scheduled_tasks WHERE id = ?",
				a.ID).Scan(&rowPrompt, &rowNative)
			if effPrompt == "" {
				effPrompt = rowPrompt.String
			}
			if effNative == "" {
				effNative = rowNative.String
			}
		}
		if reason := guardLLMCadence(effPrompt, effNative, newCronExpr, newIntervalSec, time.Now()); reason != "" {
			return tools.Fail("schedule rejected: %s", reason), nil
		}
	}

	values = append(values, a.ID)
	query := fmt.Sprintf("UPDATE of_scheduled_tasks SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, values...)
	if err != nil {
		return nil, fmt.Errorf("updating task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.NotFound("scheduled task id", fmt.Sprint(a.ID)), nil
	}
	if scheduleChanged {
		// Neither a cron nor an interval left: no next run (NULL), never a default.
		var nextRun *time.Time
		nr, ok, err := cron.NextRun(newCronExpr, newIntervalSec, time.Now())
		if err != nil {
			return tools.Fail("cron expression has no upcoming run: %s", err.Error()), nil
		}
		if ok {
			nextRun = &nr
		}
		if _, err := ctx.DB.Exec(
			"UPDATE of_scheduled_tasks SET next_run = ? WHERE id = ?",
			nextRun, a.ID,
		); err != nil {
			return tools.Fail("failed to update next_run: %v", err), nil
		}
	}
	return tools.Ok(map[string]any{"id": a.ID}), nil
}

// ScheduleDelete removes a scheduled task row.
type ScheduleDelete struct {
	ID int64 `arg:"id,required" desc:"Task id."`
}

func (a *ScheduleDelete) ActionName() string { return "delete_schedule" }
func (a *ScheduleDelete) Doc() string        { return "Delete a scheduled task." }
func (a *ScheduleDelete) Example() any       { return &ScheduleDelete{ID: 1} }
func (a *ScheduleDelete) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	res, err := ctx.DB.Exec("DELETE FROM of_scheduled_tasks WHERE id = ?", a.ID)
	if err != nil {
		return nil, fmt.Errorf("deleting task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.NotFound("scheduled task id", fmt.Sprint(a.ID)), nil
	}
	return tools.Ok(map[string]any{"deleted_id": a.ID}), nil
}

// ScheduleRunNow forces next_run to "now" so the scheduler picks up the
// task on the next tick (cron pattern is unchanged).
type ScheduleRunNow struct {
	ID int64 `arg:"id,required" desc:"Task id."`
}

func (a *ScheduleRunNow) ActionName() string { return "run_schedule_now" }
func (a *ScheduleRunNow) Doc() string {
	return "Force a scheduled task to run on the next scheduler tick (enabled tasks only)"
}
func (a *ScheduleRunNow) Example() any { return &ScheduleRunNow{ID: 1} }
func (a *ScheduleRunNow) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	res, err := ctx.DB.Exec(
		"UPDATE of_scheduled_tasks SET next_run = CURRENT_TIMESTAMP WHERE id = ? AND is_enabled = 1",
		a.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("triggering task: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.Fail("task not found or not enabled"), nil
	}
	return tools.Ok(map[string]any{
		"id":      a.ID,
		"message": "task scheduled to run on next tick (~30 seconds)",
	}), nil
}
