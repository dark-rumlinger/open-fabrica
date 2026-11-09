package engine

// Scheduled tasks: the Engine-side cron dispatcher (next_run bookkeeping,
// of_task_runs rows, in-flight dedup) and the Worker-side execution (native
// Go actions + LLM-backed prompts through the unified session loop).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/cron"
	"openfabrica/internal/funcexec"
	"openfabrica/internal/sqlguard"
	"openfabrica/internal/sqliteretry"
	"openfabrica/security"
)

// Worker-side execution

// handleScheduledTask is the CommandScheduledTask handler: native action or
// LLM prompt, then run finalization. The in-flight dedup marker is cleared
// whatever happens so the next tick is never blocked by a finished run.
func (w *Worker) handleScheduledTask(ctx context.Context, cmd Command) {
	runID := payloadInt64(cmd.Payload, "run_id")
	taskID64 := payloadInt64(cmd.Payload, "task_id")
	taskID := int(taskID64)
	defer func() {
		if taskID64 > 0 {
			w.pendingScheduledTasks.Delete(taskID64)
		}
	}()
	if nativeAction, ok := cmd.Payload["native_action"].(string); ok && nativeAction != "" {
		w.executeNativeAction(nativeAction, runID, taskID)
		return
	}
	if prompt, ok := cmd.Payload["prompt"].(string); ok && prompt != "" {
		w.executeScheduledTask(ctx, prompt, runID, taskID)
	}
}

// executeNativeAction runs a no-LLM scheduled action: delete_stale,
// count_rows, truncate, sql, or trigger_event.
// nativeSQLVerbs are the statements a schedule's sql native action may run.
var nativeSQLVerbs = []string{"SELECT", "INSERT", "UPDATE", "DELETE"}

func (w *Worker) executeNativeAction(actionJSON string, runID int64, taskID int) {
	start := time.Now()

	var action struct {
		Type         string `json:"type"`
		Table        string `json:"table"`
		Column       string `json:"column"`
		Age          string `json:"age"`
		SQL          string `json:"sql"`
		Params       []any  `json:"params"`
		EndpointPath string `json:"endpoint_path"`
	}
	if err := json.Unmarshal([]byte(actionJSON), &action); err != nil {
		w.logger.Error("native action: invalid JSON", "error", err)
		w.finalizeTaskRun(runID, taskID, false, fmt.Sprintf("invalid native_action JSON: %v", err))
		return
	}

	var result string
	var actionErr error

	switch action.Type {
	case "delete_stale":
		if action.Table == "" || action.Age == "" {
			actionErr = fmt.Errorf("delete_stale requires table and age")
			break
		}
		col := action.Column
		if col == "" {
			col = "created_at"
		}
		if security.ValidateTableName(action.Table) != nil || security.ValidateColumnName(col) != nil {
			actionErr = fmt.Errorf("invalid table or column name")
			break
		}
		query := fmt.Sprintf("DELETE FROM %s WHERE %s < datetime('now', ?)", security.QuoteIdent(action.Table), security.QuoteIdent(col))
		res, err := w.pdb.ExecWrite(query, "-"+action.Age)
		if err != nil {
			actionErr = fmt.Errorf("delete_stale: %w", err)
			break
		}
		affected, _ := res.RowsAffected()
		result = fmt.Sprintf("deleted %d rows from %s older than %s", affected, action.Table, action.Age)

	case "count_rows":
		if action.Table == "" || security.ValidateTableName(action.Table) != nil {
			actionErr = fmt.Errorf("count_rows requires a valid table")
			break
		}
		var count int
		err := w.pdb.Reader().QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", security.QuoteIdent(action.Table))).Scan(&count)
		if err != nil {
			actionErr = fmt.Errorf("count_rows: %w", err)
			break
		}
		result = fmt.Sprintf("table %s has %d rows", action.Table, count)

	case "truncate":
		if action.Table == "" || security.ValidateTableName(action.Table) != nil {
			actionErr = fmt.Errorf("truncate requires a valid table")
			break
		}
		res, err := w.pdb.ExecWrite(fmt.Sprintf("DELETE FROM %s", security.QuoteIdent(action.Table)))
		if err != nil {
			actionErr = fmt.Errorf("truncate: %w", err)
			break
		}
		affected, _ := res.RowsAffected()
		result = fmt.Sprintf("truncated %s: %d rows deleted", action.Table, affected)

	case "sql":
		q := strings.TrimSpace(action.SQL)
		// The guard every stored SQL action shares; scheduled cleanup also admits DELETE.
		verb, err := sqlguard.CheckAction(q, nativeSQLVerbs...)
		if err != nil {
			actionErr = fmt.Errorf("sql: %w", err)
			break
		}
		switch verb {
		case "SELECT":
			rows, err := w.pdb.Reader().Query(q, action.Params...)
			if err != nil {
				actionErr = fmt.Errorf("sql query: %w", err)
				break
			}
			cols, _ := rows.Columns()
			var count int
			for rows.Next() {
				count++
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				actionErr = fmt.Errorf("sql query: %w", err)
				break
			}
			result = fmt.Sprintf("query returned %d rows (%d columns)", count, len(cols))
		case "DELETE", "UPDATE", "INSERT":
			res, err := w.pdb.ExecWrite(q, action.Params...)
			if err != nil {
				actionErr = fmt.Errorf("sql exec: %w", err)
				break
			}
			affected, _ := res.RowsAffected()
			result = fmt.Sprintf("sql executed: %d rows affected", affected)
		default:
			actionErr = fmt.Errorf("sql action only supports SELECT, INSERT, UPDATE, DELETE as the leading verb; a CTE (WITH ...) is not accepted here")
		}

	case "trigger_event":
		var triggerCfg struct {
			Type      string         `json:"type"`
			EventType string         `json:"event_type"`
			Payload   map[string]any `json:"payload"`
		}
		if err := json.Unmarshal([]byte(actionJSON), &triggerCfg); err != nil {
			actionErr = fmt.Errorf("trigger_event: invalid config: %w", err)
			break
		}
		if triggerCfg.EventType == "" {
			actionErr = fmt.Errorf("trigger_event: event_type is required")
			break
		}
		if triggerCfg.Payload == nil {
			triggerCfg.Payload = map[string]any{}
		}
		triggerCfg.Payload["source"] = "scheduler"
		triggerCfg.Payload["task_id"] = taskID
		w.publishEvent(events.EventType(triggerCfg.EventType), triggerCfg.Payload)
		result = fmt.Sprintf("published event '%s'", triggerCfg.EventType)
		// The event type is a free string, so a typo publishes an event nobody
		// matches and the task "succeeds" doing nothing. Make that visible: if
		// no enabled trigger listens on this name, say so in the task result
		// and the log.
		if warn := noListenerWarning(w.pdb, triggerCfg.EventType); warn != "" {
			result += " - " + warn
			w.logger.Warn("scheduled trigger_event fired with no listeners",
				"event_type", triggerCfg.EventType, "task_id", taskID)
		}

	case "run_function":
		if action.EndpointPath == "" {
			actionErr = fmt.Errorf("run_function requires endpoint_path")
			break
		}
		result, actionErr = w.runScheduledFunction(action.EndpointPath, taskID)

	default:
		actionErr = fmt.Errorf("unknown native action type: %s", action.Type)
	}

	duration := time.Since(start)
	if actionErr != nil {
		w.logger.Error("native action failed", "type", action.Type, "error", actionErr, "duration", duration)
		w.finalizeTaskRun(runID, taskID, false, actionErr.Error())
		return
	}

	w.logger.Info("native action completed", "type", action.Type, "result", result, "duration", duration)
	if w.run != nil {
		w.runlog.entry(w.run.ID, RunLogMessage, "scheduled_task_native",
			map[string]string{"text": result, "action": action.Type}, 0, 0, duration.Milliseconds())
	}
	w.finalizeTaskRun(runID, taskID, true, "")
}

// runScheduledFunction invokes a manage_function handler as a scheduled native action.
// It runs the stored handler through the shared funcexec path - the SAME sandbox, guards
// (SELECT-only ctx.query, SSRF-checked ctx.fetch, secret allowlist), and declared-write
// policy the HTTP endpoint and run_function trigger use - so cron-driven function logic
// can't drift from the other two call paths. Scheduler-flavoured: req.body carries the
// source + task id, no HTTP caller.
func (w *Worker) runScheduledFunction(endpointPath string, taskID int) (string, error) {
	var code, secrets string
	var timeoutMS int
	err := w.pdb.Reader().QueryRow(
		"SELECT code, COALESCE(secrets,''), COALESCE(timeout_ms,0) FROM of_function_endpoints WHERE path = ?",
		endpointPath,
	).Scan(&code, &secrets, &timeoutMS)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no function endpoint at %q", endpointPath)
	}
	if err != nil {
		return "", fmt.Errorf("loading function %q: %w", endpointPath, err)
	}

	spec := funcexec.Spec{Code: code, SecretsJSON: secrets}
	if timeoutMS > 0 {
		spec.Timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	req := map[string]any{
		"method":  "schedule",
		"path":    "/schedule",
		"query":   map[string]any{},
		"headers": map[string]any{},
		"body":    map[string]any{"source": "scheduler", "task_id": taskID},
		"user":    nil,
	}
	rt := funcexec.NewRuntime(w.projectID, w.pdb.Reader(), w.pdb.Writer(), w.completeDeps())
	resp, err := funcexec.Execute(rt, spec, req)
	if err != nil {
		return "", err
	}
	status := 200
	if resp != nil {
		status = resp.Status
	}
	return fmt.Sprintf("ran function %q (status %d)", endpointPath, status), nil
}

// noListenerWarning returns a warning string when no enabled trigger listens
// on eventType, or "" when at least one does (or the lookup itself fails - a
// read error must not turn a successful publish into a scary warning).
func noListenerWarning(pdb *db.ProjectDB, eventType string) string {
	var listeners int
	if err := pdb.Reader().QueryRow(
		"SELECT COUNT(*) FROM of_triggers WHERE event_type = ? AND is_enabled = 1",
		eventType,
	).Scan(&listeners); err != nil || listeners > 0 {
		return ""
	}
	return fmt.Sprintf("WARNING: no enabled trigger listens on event type '%s' (typo?)", eventType)
}

// executeScheduledTask runs an LLM-backed scheduled task: one live-spec
// session over the prompt. The live thread is NOT carried in or out - a
// scheduled run is its own conversation, not part of the owner's chat.
func (w *Worker) executeScheduledTask(ctx context.Context, prompt string, runID int64, taskID int) {
	// Starting is a fresh intent to run: drop a stop left set by an earlier
	// session that ended before the between-turns check could consume it, so it
	// cannot end this task at iteration 0. Mirrors drive() and handleChatWake.
	w.clearStopRequest()

	spec := w.scheduledTaskSpec(prompt)
	sess := w.newStageSession(StageLive)
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		w.finalizeTaskRun(runID, taskID, false, fmt.Sprintf("LLM error: %v", err))
		return
	}
	if w.run != nil {
		w.runlog.entry(w.run.ID, RunLogMessage, "scheduled_task",
			map[string]string{"text": res.Content, "prompt": prompt},
			res.InputTokens, res.OutputTokens, 0)
	}
	// An owner stop is neither a success nor the task's failure, and it is not
	// evidence the task is a no-op: recording it as a completed run would bump
	// run_count for work that never happened, and letting it reach the idle
	// tracker would extend a streak that auto-disables the task.
	if res.EndDetail == endOwnerStop.detail {
		w.clearStopRequest()
		w.finalizeStoppedTaskRun(runID)
		w.publishStopLanded("scheduled")
		return
	}
	w.finalizeTaskRun(runID, taskID, true, "")
	if res.Paused {
		// The task asked the owner something. Park like a LIVE question so the
		// answer re-enters the agent; it is neither idle nor a failed run.
		w.publishMessage(pickPhrase(
			"A scheduled task needs your answer before it can carry on.",
			"One of my scheduled tasks has a question for you.",
		))
		w.stampLiveQuestions()
		w.pauseAwaitingAnswers()
		return
	}
	w.trackScheduledIdle(res, taskID)
}

// scheduledTaskSpec assembles the session spec for one prompt-driven scheduled run: the
// LIVE prompt and tool set, with no carried chat thread.
func (w *Worker) scheduledTaskSpec(prompt string) SessionSpec {
	spec := DefaultSpec(StageLive)
	spec.Guard = &RepeatGuard{}
	// Must match liveSystemPrompt's preloaded set, which it advertises as loaded.
	spec.PreloadTools = livePreloadTools()
	spec.System = w.liveSystemPrompt()
	spec.Seed = seed{
		owner: w.ownerContext() + w.ownerMemoryBlock(),
		task:  "[mode: scheduled-task] " + prompt,
	}.messages()
	spec.Reseed = liveReseed(spec.Seed)
	return spec
}

// maxIdleScheduledRuns is how many consecutive zero-write prompt runs a schedule
// may rack up before it is auto-disabled - with the one-hour cadence floor,
// roughly that many HOURS of doing nothing: generous for a real periodic task,
// a firm stop for a pure heartbeat that would wake the LLM forever.
const maxIdleScheduledRuns = 10

// nextIdleCount advances the consecutive-idle counter for one scheduled run.
// A run that wrote something resets the streak (0, keep running); a run that
// wrote nothing extends it, and once it reaches maxIdleScheduledRuns the task
// should be disabled. Pure, so the backoff decision is unit-tested directly.
func nextIdleCount(prev int, wrote bool) (count int, disable bool) {
	if wrote {
		return 0, false
	}
	count = prev + 1
	return count, count >= maxIdleScheduledRuns
}

// trackScheduledIdle is the no-op backoff: a run that performed no write counts
// as idle, a write resets the count, and after maxIdleScheduledRuns consecutive
// idle runs the task is disabled with the owner told why - re-enabling stays
// possible, since a genuine monitor may simply be quiet for a long stretch.
func (w *Worker) trackScheduledIdle(res SessionResult, taskID int) {
	if taskID <= 0 {
		return
	}
	// The counter is persisted on of_scheduled_tasks so the backoff survives a
	// process restart instead of resetting to zero every time.
	prev := 0
	_ = w.pdb.Reader().QueryRow(
		"SELECT COALESCE(idle_run_count, 0) FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&prev)

	count, disable := nextIdleCount(prev, res.Writes > 0)
	if !disable {
		// Persist the new count (count == 0 when a run wrote something, which
		// resets the streak).
		if _, err := w.pdb.ExecWrite(
			"UPDATE of_scheduled_tasks SET idle_run_count = ? WHERE id = ?", count, taskID); err != nil {
			w.logger.Warn("no-op backoff: failed to persist idle count", "task_id", taskID, "error", err)
		}
		return
	}

	// Threshold hit: disable the task, reset the counter (so a re-enable starts
	// fresh), and tell the owner.
	var name string
	_ = w.pdb.Reader().QueryRow("SELECT name FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&name)
	if _, err := w.pdb.ExecWrite(
		"UPDATE of_scheduled_tasks SET is_enabled = 0, idle_run_count = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?", taskID); err != nil {
		w.logger.Warn("no-op backoff: failed to disable idle scheduled task", "task_id", taskID, "error", err)
		return
	}
	w.logger.Info("no-op backoff: disabled idle scheduled task", "task_id", taskID, "name", name, "idle_runs", count)
	label := name
	if label == "" {
		label = fmt.Sprintf("#%d", taskID)
	}
	w.publishMessage(fmt.Sprintf(
		"I paused the scheduled task **%s** - it ran %d times in a row without changing anything, so it was only spending tokens to report that all is well. Data persists on write, so a heartbeat/health-check usually isn't needed. If you want it back, re-enable it in Settings (or ask me), and consider running it less often or as a no-LLM action.",
		label, count))
}

// finalizeTaskRun updates the of_task_runs row and publishes completion or
// failure events.
func (w *Worker) finalizeTaskRun(runID int64, taskID int, success bool, errMsg string) {
	if runID <= 0 {
		return
	}
	if success {
		if _, err := w.pdb.ExecWrite(
			"UPDATE of_task_runs SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE id = ?", runID); err != nil {
			w.logger.Warn("finalize task run failed", "error", err)
		}
		if taskID > 0 {
			_, _ = w.pdb.ExecWrite("UPDATE of_scheduled_tasks SET run_count = run_count + 1 WHERE id = ?", taskID)
		}
		w.publishEvent(events.EventScheduledCompleted, map[string]any{
			"task_id": taskID,
			"run_id":  runID,
		})
		return
	}
	if _, err := w.pdb.ExecWrite(
		"UPDATE of_task_runs SET status = 'failed', error_message = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?", errMsg, runID); err != nil {
		w.logger.Warn("finalize task run failed", "error", err)
	}
	if taskID > 0 {
		_, _ = w.pdb.ExecWrite("UPDATE of_scheduled_tasks SET error_count = error_count + 1 WHERE id = ?", taskID)
	}
	w.publishEvent(events.EventScheduledFailed, map[string]any{
		"task_id": taskID,
		"run_id":  runID,
		"error":   errMsg,
	})
}

// finalizeStoppedTaskRun closes the of_task_runs row for a run the owner
// stopped. Deliberately neither of finalizeTaskRun's branches: nobody is at
// fault, so run_count and error_count both stay put rather than skewing counters
// the owner reads as a health signal, and no scheduled.* event follows - a stop
// is the owner's own action, already visible in the chat.
func (w *Worker) finalizeStoppedTaskRun(runID int64) {
	if runID <= 0 {
		return
	}
	if _, err := w.pdb.ExecWrite(
		"UPDATE of_task_runs SET status = 'stopped', error_message = 'stopped by owner', completed_at = CURRENT_TIMESTAMP WHERE id = ?",
		runID); err != nil {
		w.logger.Warn("finalize stopped task run failed", "error", err)
	}
}

// Engine-side dispatcher

const schedulerTickInterval = 30 * time.Second

// schedulerLoop initializes next_run for fresh tasks, then ticks: due tasks
// get an of_task_runs row, a CommandScheduledTask dispatch, and their
// next_run/last_run advanced (cron expression or fixed interval).
func (e *Engine) schedulerLoop() {
	e.initializeNextRun()

	ticker := time.NewTicker(schedulerTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.rootCtx.Done():
			return
		case <-ticker.C:
			e.checkDueTasks()
		}
	}
}

// forEachRunningProject opens each running project's DB and invokes fn, skipping
// any project whose DB fails to open.
func (e *Engine) forEachRunningProject(fn func(pdb *db.ProjectDB, projectID int)) {
	for _, projectID := range e.RunningProjects() {
		pdb, err := e.deps.ProjectDBManager.Open(projectID)
		if err != nil {
			continue
		}
		fn(pdb, projectID)
	}
}

func (e *Engine) initializeNextRun() {
	e.forEachRunningProject(func(pdb *db.ProjectDB, _ int) {
		e.initializeNextRunForProject(pdb)
	})
}

// computeNextRun resolves a scheduled task's next fire time from its cron
// expression or interval. ok is false when the task has neither (a spent
// one-shot). A non-nil error means the cron expression is invalid and the task
// must be disabled rather than retried.
func computeNextRun(cronExpr sql.NullString, intervalSec sql.NullInt64, now time.Time) (next time.Time, ok bool, err error) {
	return cron.NextRun(cronExpr.String, int(intervalSec.Int64), now)
}

func (e *Engine) initializeNextRunForProject(pdb *db.ProjectDB) {
	rows, err := pdb.Query(
		`SELECT id, cron_expression, interval_seconds FROM of_scheduled_tasks
		 WHERE is_enabled = 1 AND next_run IS NULL`)
	if err != nil {
		e.logger.Error("scheduler: failed to query uninitialized tasks", "project_id", pdb.ProjectID, "error", err)
		return
	}
	type taskInit struct {
		id          int
		cronExpr    sql.NullString
		intervalSec sql.NullInt64
	}
	var tasks []taskInit
	for rows.Next() {
		var t taskInit
		if rows.Scan(&t.id, &t.cronExpr, &t.intervalSec) == nil {
			tasks = append(tasks, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		e.logger.Error("scheduler: failed to read uninitialized tasks", "project_id", pdb.ProjectID, "error", err)
		return
	}

	now := time.Now()
	for _, t := range tasks {
		nextRun, ok, err := computeNextRun(t.cronExpr, t.intervalSec, now)
		if err != nil {
			// A malformed/impossible expression must not fire; disable it
			// rather than leave it to be retried (and never run) forever.
			e.logger.Error("scheduler: invalid cron expression, disabling task", "project_id", pdb.ProjectID, "task_id", t.id, "cron", t.cronExpr.String, "error", err)
			_, _ = pdb.ExecWrite("UPDATE of_scheduled_tasks SET is_enabled = 0 WHERE id = ?", t.id)
			continue
		}
		if !ok {
			continue
		}
		if _, err := pdb.ExecWrite("UPDATE of_scheduled_tasks SET next_run = ? WHERE id = ?", nextRun, t.id); err != nil {
			e.logger.Error("scheduler: failed to initialize next_run", "project_id", pdb.ProjectID, "task_id", t.id, "error", err)
		}
	}
}

func (e *Engine) checkDueTasks() {
	now := time.Now()
	e.forEachRunningProject(func(pdb *db.ProjectDB, projectID int) {
		e.checkDueTasksForProject(pdb, projectID, now)
	})
}

func (e *Engine) checkDueTasksForProject(pdb *db.ProjectDB, projectID int, now time.Time) {
	rows, err := sqliteretry.Query(pdb.Reader(),
		`SELECT id, name, COALESCE(prompt,''), cron_expression, interval_seconds, COALESCE(native_action,'')
		 FROM of_scheduled_tasks
		 WHERE is_enabled = 1 AND next_run IS NOT NULL AND next_run <= ?`,
		now)
	if err != nil {
		e.logger.Error("scheduler: failed to query due tasks", "project_id", projectID, "error", err)
		return
	}
	type dueTask struct {
		id           int
		name         string
		prompt       string
		cronExpr     sql.NullString
		intervalSec  sql.NullInt64
		nativeAction string
	}
	var tasks []dueTask
	for rows.Next() {
		var t dueTask
		if rows.Scan(&t.id, &t.name, &t.prompt, &t.cronExpr, &t.intervalSec, &t.nativeAction) == nil {
			tasks = append(tasks, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		// Dispatch nothing this tick: the tasks stay due (next_run is only
		// advanced on dispatch), so the next tick retries the full set.
		e.logger.Error("scheduler: failed to read due tasks", "project_id", projectID, "error", err)
		return
	}

	for _, t := range tasks {
		e.logger.Info("dispatching scheduled task", "task_id", t.id, "task_name", t.name, "project_id", projectID)

		runResult, err := pdb.ExecWrite("INSERT INTO of_task_runs (task_id, status) VALUES (?, 'running')", t.id)
		var runID int64
		if err == nil {
			runID, _ = runResult.LastInsertId()
		}

		err = e.Send(projectID, Command{
			Type:      CommandScheduledTask,
			ProjectID: projectID,
			Payload: map[string]any{
				"prompt":        t.prompt,
				"task_id":       int64(t.id),
				"run_id":        runID,
				"native_action": t.nativeAction,
			},
		})
		if err != nil {
			// In-flight dedup is an expected skip whenever a task's run time
			// exceeds its interval; mark the run but don't surface an error.
			if errors.Is(err, ErrScheduledTaskPending) {
				e.logger.Info("scheduler: task already in flight, skipping tick", "task_id", t.id)
				if runID > 0 {
					_, _ = pdb.ExecWrite(
						"UPDATE of_task_runs SET status = 'skipped', error_message = 'previous run still in flight', completed_at = CURRENT_TIMESTAMP WHERE id = ?",
						runID)
				}
			} else {
				e.logger.Error("scheduler: failed to dispatch task", "task_id", t.id, "error", err)
				if runID > 0 {
					_, _ = pdb.ExecWrite(
						"UPDATE of_task_runs SET status = 'failed', error_message = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ?",
						fmt.Sprintf("dispatch error: %v", err), runID)
				}
			}
		}

		// Advance the schedule regardless of dispatch outcome so a stuck
		// worker can't pile up an unbounded backlog of due ticks.
		nextRun, ok, err := computeNextRun(t.cronExpr, t.intervalSec, now)
		if err != nil {
			// Can't compute the next fire: disable rather than re-fire with
			// a garbage time (or spin every tick).
			e.logger.Error("scheduler: invalid cron expression, disabling task", "project_id", projectID, "task_id", t.id, "cron", t.cronExpr.String, "error", err)
			_, _ = pdb.ExecWrite("UPDATE of_scheduled_tasks SET is_enabled = 0, last_run = ? WHERE id = ?", now, t.id)
			continue
		}
		if !ok {
			// One-shot task (no schedule left): disable after firing once.
			_, _ = pdb.ExecWrite("UPDATE of_scheduled_tasks SET is_enabled = 0, last_run = ? WHERE id = ?", now, t.id)
			continue
		}
		_, _ = pdb.ExecWrite("UPDATE of_scheduled_tasks SET last_run = ?, next_run = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?", now, nextRun, t.id)
	}
}
