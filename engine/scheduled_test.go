package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/events"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// scheduledTestWorker builds a worker against temp DBs with the fake provider.
func scheduledTestWorker(t *testing.T, p *enginetest.Provider) (*Worker, *db.ProjectDB) {
	t.Helper()
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { gdb.Close() })
	mgr := db.NewProjectDBManager(dir)
	t.Cleanup(mgr.CloseAll)
	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id)
		 VALUES (1, 'P', 'desc', 1, 'live', 1)`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     tools.DefaultRegistry(),
		ToolExecutor:     tools.NewExecutor(tools.DefaultRegistry()),
	}
	w, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	t.Cleanup(func() { w.dbLog.Close() })
	if p != nil {
		w.providerOverride = staticProviderSource{rp: resolvedProvider{
			Provider: p, Model: "fake-model", ProviderID: 1}}
	}
	pdb, _ := mgr.Open(1)
	return w, pdb
}

// seedScheduledTask inserts one of_scheduled_tasks row + a running
// of_task_runs row, returning (taskID, runID).
func seedScheduledTask(t *testing.T, pdb *db.ProjectDB, prompt, nativeAction string) (int64, int64) {
	t.Helper()
	res, err := pdb.ExecWrite(
		`INSERT INTO of_scheduled_tasks (name, prompt, native_action, interval_seconds, is_enabled)
		 VALUES ('test task', ?, ?, 60, 1)`, prompt, nativeAction)
	if err != nil {
		t.Fatalf("insert scheduled task: %v", err)
	}
	taskID, _ := res.LastInsertId()
	runRes, err := pdb.ExecWrite("INSERT INTO of_task_runs (task_id, status) VALUES (?, 'running')", taskID)
	if err != nil {
		t.Fatalf("insert task run: %v", err)
	}
	runID, _ := runRes.LastInsertId()
	return taskID, runID
}

func taskRunStatus(t *testing.T, pdb *db.ProjectDB, runID int64) (status, errMsg string) {
	t.Helper()
	if err := pdb.QueryRow(
		"SELECT status, COALESCE(error_message,'') FROM of_task_runs WHERE id = ?", runID,
	).Scan(&status, &errMsg); err != nil {
		t.Fatalf("read task run: %v", err)
	}
	return status, errMsg
}

// TestScheduledNativeAction runs a native count_rows action end-to-end:
// the run completes and the task's run_count bumps.
func TestScheduledNativeAction(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	taskID, runID := seedScheduledTask(t, pdb, "", `{"type":"count_rows","table":"of_memory"}`)

	w.handleScheduledTask(context.Background(), Command{
		Type: CommandScheduledTask,
		Payload: map[string]any{
			"task_id":       taskID,
			"run_id":        runID,
			"native_action": `{"type":"count_rows","table":"of_memory"}`,
		},
	})

	status, errMsg := taskRunStatus(t, pdb, runID)
	if status != "completed" {
		t.Fatalf("run status = %s (%s), want completed", status, errMsg)
	}
	var runCount int
	_ = pdb.QueryRow("SELECT run_count FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&runCount)
	if runCount != 1 {
		t.Fatalf("run_count = %d, want 1", runCount)
	}
}

// TestScheduledNativeActionRejectsBadTable: invalid table names fail the run
// (never reach SQL) and bump error_count.
func TestScheduledNativeActionRejectsBadTable(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	action := `{"type":"truncate","table":"of_memory; DROP TABLE of_files"}`
	taskID, runID := seedScheduledTask(t, pdb, "", action)

	w.handleScheduledTask(context.Background(), Command{
		Type: CommandScheduledTask,
		Payload: map[string]any{
			"task_id":       taskID,
			"run_id":        runID,
			"native_action": action,
		},
	})

	status, errMsg := taskRunStatus(t, pdb, runID)
	if status != "failed" || errMsg == "" {
		t.Fatalf("run status = %s (%q), want failed with reason", status, errMsg)
	}
	var errCount int
	_ = pdb.QueryRow("SELECT error_count FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&errCount)
	if errCount != 1 {
		t.Fatalf("error_count = %d, want 1", errCount)
	}
}

// TestScheduledTriggerEventNoListenerWarning: a trigger_event action's event
// type is a free string, so a typo publishes an event nobody matches and the
// task "succeeds" doing nothing. noListenerWarning makes that visible - and
// stays quiet once an enabled trigger listens on the name.
func TestScheduledTriggerEventNoListenerWarning(t *testing.T) {
	_, pdb := scheduledTestWorker(t, nil)

	if warn := noListenerWarning(pdb, "custom.daily_report"); warn == "" {
		t.Fatal("want a no-listener warning for an event type with no triggers")
	} else if !strings.Contains(warn, "custom.daily_report") {
		t.Fatalf("warning should name the event type, got %q", warn)
	}

	if _, err := pdb.ExecWrite(
		`INSERT INTO of_triggers (name, event_type, trigger_type, trigger_config, is_enabled)
		 VALUES ('report mail', 'custom.daily_report', 'send_email', '{}', 1)`); err != nil {
		t.Fatalf("insert trigger: %v", err)
	}
	if warn := noListenerWarning(pdb, "custom.daily_report"); warn != "" {
		t.Fatalf("no warning expected once an enabled trigger listens, got %q", warn)
	}

	// A disabled listener does not count.
	if _, err := pdb.ExecWrite(
		"UPDATE of_triggers SET is_enabled = 0 WHERE event_type = 'custom.daily_report'"); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if warn := noListenerWarning(pdb, "custom.daily_report"); warn == "" {
		t.Fatal("want a warning again once the only listener is disabled")
	}
}

// TestScheduledLLMTask runs an LLM-backed scheduled prompt through the
// session loop with the fake provider.
func TestScheduledLLMTask(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("checked the feed, nothing to do"),
	}}
	w, pdb := scheduledTestWorker(t, p)
	taskID, runID := seedScheduledTask(t, pdb, "check the feed for new items", "")

	w.handleScheduledTask(context.Background(), Command{
		Type: CommandScheduledTask,
		Payload: map[string]any{
			"task_id": taskID,
			"run_id":  runID,
			"prompt":  "check the feed for new items",
		},
	})

	status, errMsg := taskRunStatus(t, pdb, runID)
	if status != "completed" {
		t.Fatalf("run status = %s (%s), want completed", status, errMsg)
	}
	// A text-only first turn is a finished answer in a LIVE-stage session (no
	// iteration-0 re-prompt), so exactly one provider call.
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.CallCount())
	}
	// The scheduled prompt reached the model with its mode tag.
	found := false
	for _, m := range p.Requests[0].Messages {
		if contains(m.Content, "[mode: scheduled-task] check the feed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("prompt not framed for scheduled mode in the first request")
	}
}

// TestScheduledTaskSpecLoadsWhatItsPromptAdvertises: the scheduled run reuses the LIVE
// system prompt, which lists manage_codebase / manage_diagnostics as always-loaded and
// leaves them out of the deferred catalogue. The spec must actually preload them, or a
// call the prompt invites comes back "not loaded yet".
func TestScheduledTaskSpecLoadsWhatItsPromptAdvertises(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_memory (key, value, category) VALUES ('spelling', 'British English', 'preferences')`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	spec := w.scheduledTaskSpec("send the weekly digest")

	have := map[string]bool{}
	for _, n := range spec.PreloadTools {
		have[n] = true
	}
	for _, n := range livePreloadTools() {
		if !have[n] {
			t.Errorf("scheduled spec does not preload %q, which the LIVE prompt advertises as loaded", n)
		}
	}
	for _, n := range tools.LiveExtraToolNames {
		if !have[n] {
			t.Errorf("scheduled spec missing live-only tool %q", n)
		}
	}

	// Owner preferences apply to scheduled work too (a digest in the owner's spelling).
	found := false
	for _, m := range spec.Seed {
		if strings.Contains(m.Content, "spelling: British English") {
			found = true
		}
	}
	if !found {
		t.Error("scheduled seed does not carry the owner's stored preferences")
	}
}

// TestLiveQuestionAnsweredWithoutToolCall: an owner question in LIVE ("what do you
// suggest?") is answered in chat, and the live frame forbids editing until the owner
// picks. A text-only first turn must end the wake, not draw a "you must call a tool"
// re-prompt that pushes the model into acting on a question.
func TestLiveQuestionAnsweredWithoutToolCall(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("Two ideas: a seasonal menu banner, or a reservations form. Which would you like?"),
		enginetest.Text("must not be requested"),
	}}
	w, _ := scheduledTestWorker(t, p)
	w.handleChatWake(context.Background(), "any ideas for the homepage?")
	if p.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (a text answer ends a LIVE wake)", p.CallCount())
	}
}

// scheduledCounters reads the health counters a scheduled run can move. An
// owner stop must move none of them.
func scheduledCounters(t *testing.T, pdb *db.ProjectDB, taskID int64) (runs, errs, idle int) {
	t.Helper()
	if err := pdb.QueryRow(
		`SELECT COALESCE(run_count,0), COALESCE(error_count,0), COALESCE(idle_run_count,0)
		 FROM of_scheduled_tasks WHERE id = ?`, taskID).Scan(&runs, &errs, &idle); err != nil {
		t.Fatalf("read task counters: %v", err)
	}
	return runs, errs, idle
}

// TestScheduledTaskOwnerStopIsNotSuccess: a stop ends the session at iteration 0
// having bought nothing, and recording that as a completed run is wrong three
// times over - it bumps run_count for work that never happened, and the zero
// writes look like a no-op run, which extends the idle streak that eventually
// auto-disables the task. An owner stop is nobody's failure either, so
// error_count must stay put as well.
func TestScheduledTaskOwnerStopIsNotSuccess(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	taskID, runID := seedScheduledTask(t, pdb, "check the feed for new items", "")

	// The stop lands while the first turn is in flight, so the session ends at
	// the next boundary rather than running the task out.
	scripted := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files", Arguments: `{"action":"list"}`}),
		enginetest.Text("must not run"),
	}}
	w.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: &stopAfterCall{Provider: scripted, after: 1, fire: w.requestStop},
		Model:    "fake-model", ProviderID: 1}}

	w.deps.Bus = events.NewBus()
	var landed []string
	w.deps.Bus.SubscribeAll(func(e events.Event) {
		if e.Type == events.EventEngineStopLanded {
			reason, _ := e.Payload["reason"].(string)
			landed = append(landed, reason)
		}
	})

	w.handleScheduledTask(context.Background(), Command{
		Type: CommandScheduledTask,
		Payload: map[string]any{
			"task_id": taskID,
			"run_id":  runID,
			"prompt":  "check the feed for new items",
		},
	})

	if status, _ := taskRunStatus(t, pdb, runID); status != "stopped" {
		t.Fatalf("run status = %s, want stopped: an owner stop is neither a success nor a failure", status)
	}
	runs, errs, idle := scheduledCounters(t, pdb, taskID)
	if runs != 0 || errs != 0 || idle != 0 {
		t.Fatalf("counters run=%d err=%d idle=%d, want all 0: a stop must not move the task's health counters",
			runs, errs, idle)
	}
	if len(landed) != 1 || landed[0] != "scheduled" {
		t.Fatalf("stop_landed reasons = %v, want exactly one \"scheduled\"", landed)
	}
	if w.stopRequested() {
		t.Fatal("stop request survived the task run")
	}
}

// TestScheduledTaskDropsStaleStopRequest: a scheduled task is a fresh intent to
// run, so a request left set by an earlier session must not kill it on arrival.
func TestScheduledTaskDropsStaleStopRequest(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text("checked the feed, nothing to do"),
	}}
	w, pdb := scheduledTestWorker(t, p)
	taskID, runID := seedScheduledTask(t, pdb, "check the feed", "")

	w.requestStop() // nothing consumed it; it is stale by the time the task runs

	w.handleScheduledTask(context.Background(), Command{
		Type: CommandScheduledTask,
		Payload: map[string]any{
			"task_id": taskID,
			"run_id":  runID,
			"prompt":  "check the feed",
		},
	})

	if p.CallCount() == 0 {
		t.Fatal("a stale stop request killed the scheduled task before it bought a turn")
	}
	if status, _ := taskRunStatus(t, pdb, runID); status != "completed" {
		t.Fatalf("run status = %s, want completed", status)
	}
}

// TestScheduledTaskDedup: a second tick for an in-flight task is rejected
// with ErrScheduledTaskPending, and the marker clears after execution.
func TestScheduledTaskDedup(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	taskID, runID := seedScheduledTask(t, pdb, "", `{"type":"count_rows","table":"of_memory"}`)

	cmd := Command{
		Type: CommandScheduledTask,
		Payload: map[string]any{
			"task_id":       taskID,
			"run_id":        runID,
			"native_action": `{"type":"count_rows","table":"of_memory"}`,
		},
	}
	if err := w.Send(cmd); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := w.Send(cmd); !errors.Is(err, ErrScheduledTaskPending) {
		t.Fatalf("second send = %v, want ErrScheduledTaskPending", err)
	}

	// Execute (drains the marker), then a new tick is accepted again.
	w.handleScheduledTask(context.Background(), cmd)
	if err := w.Send(cmd); err != nil {
		t.Fatalf("post-execution send = %v, want accepted", err)
	}
}
