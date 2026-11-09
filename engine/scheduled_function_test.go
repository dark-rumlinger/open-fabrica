package engine

import (
	"context"
	"testing"
)

// TestScheduledRunFunction drives the run_function native action: a cron/interval
// task invokes a stored manage_function handler, whose declared write lands.
func TestScheduledRunFunction(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	if _, err := pdb.ExecWrite(`CREATE TABLE rates (id INTEGER PRIMARY KEY, pair TEXT, val INTEGER)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	code := `function handler(req, ctx){ ctx.insert('rates', { pair: 'EURUSD', val: 108 }); return { ok: true }; }`
	if _, err := pdb.ExecWrite(`INSERT INTO of_function_endpoints (path, code, methods) VALUES ('sync-rates', ?, 'POST')`, code); err != nil {
		t.Fatalf("seed function: %v", err)
	}
	action := `{"type":"run_function","endpoint_path":"sync-rates"}`
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
	if status != "completed" {
		t.Fatalf("run status = %s (%s), want completed", status, errMsg)
	}
	var pair string
	var val int
	if err := pdb.QueryRow("SELECT pair, val FROM rates ORDER BY id DESC LIMIT 1").Scan(&pair, &val); err != nil {
		t.Fatalf("read rates: %v", err)
	}
	if pair != "EURUSD" || val != 108 {
		t.Fatalf("scheduled function write missing: pair=%q val=%d", pair, val)
	}
}

// TestScheduledRunFunctionMissingEndpoint: a run_function pointing at a
// nonexistent endpoint fails the run with a reason (not a silent success).
func TestScheduledRunFunctionMissingEndpoint(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	action := `{"type":"run_function","endpoint_path":"nope"}`
	taskID, runID := seedScheduledTask(t, pdb, "", action)

	w.handleScheduledTask(context.Background(), Command{
		Type:    CommandScheduledTask,
		Payload: map[string]any{"task_id": taskID, "run_id": runID, "native_action": action},
	})

	status, errMsg := taskRunStatus(t, pdb, runID)
	if status != "failed" || errMsg == "" {
		t.Fatalf("run status = %s (%q), want failed with reason", status, errMsg)
	}
}
