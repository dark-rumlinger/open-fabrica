package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// runScheduleForFlow runs a project's scheduled task for a user-flow replay's
// runSchedule step ("the nightly digest goes out", "stale drafts are cleaned") and
// waits for it, recording the run in of_task_runs like a tick would. The native
// action runs right here rather than as a worker command: the replay usually runs
// inside that worker (a build's test_flows or its validators), so a queued command
// would only run after the replay gave up waiting. A prompt task is refused: it
// wakes the whole agent, which a replay must never do.
func (e *Engine) runScheduleForFlow(ctx context.Context, projectID int, name string) error {
	e.mu.RLock()
	h := e.workers[projectID]
	e.mu.RUnlock()
	if h == nil || h.worker == nil {
		return errors.New("the project is not running, so its scheduled tasks cannot run")
	}
	w := h.worker
	var (
		id             int
		native, prompt string
	)
	err := w.pdb.Reader().QueryRow(
		`SELECT id, COALESCE(native_action,''), COALESCE(prompt,'') FROM of_scheduled_tasks WHERE name = ?`, name,
	).Scan(&id, &native, &prompt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("the project has no scheduled task named %q", name)
	}
	if err != nil {
		return err
	}
	if native == "" {
		return errors.New("it runs an agent prompt, which a replay cannot start; check what the task produces another way, or make it a native_action")
	}
	res, err := w.pdb.ExecWrite("INSERT INTO of_task_runs (task_id, status) VALUES (?, 'running')", id)
	if err != nil {
		return err
	}
	runID, _ := res.LastInsertId()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.executeNativeAction(native, runID, id)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return errors.New("it did not finish in time")
	}
	var status, msg string
	if err := w.pdb.Reader().QueryRow(
		`SELECT status, COALESCE(error_message,'') FROM of_task_runs WHERE id = ?`, runID,
	).Scan(&status, &msg); err != nil {
		return err
	}
	if status == "failed" {
		return fmt.Errorf("it failed: %s", msg)
	}
	return nil
}
