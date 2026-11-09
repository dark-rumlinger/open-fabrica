package engine

import (
	"testing"

	"openfabrica/db"
)

// The no-op backoff: consecutive zero-write prompt runs accumulate until the
// task is disabled; any run that writes resets the streak.
func TestNextIdleCount(t *testing.T) {
	// A run that wrote something always resets and never disables.
	if c, d := nextIdleCount(maxIdleScheduledRuns-1, true); c != 0 || d {
		t.Fatalf("a writing run must reset (0, false), got (%d, %v)", c, d)
	}

	// Idle runs accumulate but do not disable until the threshold.
	count := 0
	disabledAt := 0
	for run := 1; run <= maxIdleScheduledRuns+3; run++ {
		var disable bool
		count, disable = nextIdleCount(count, false)
		if disable {
			disabledAt = run
			break
		}
	}
	if disabledAt != maxIdleScheduledRuns {
		t.Fatalf("a pure heartbeat must disable on run %d, disabled on %d", maxIdleScheduledRuns, disabledAt)
	}

	// A single write midway resets the streak, so it takes the full threshold
	// again from there - a genuinely-working task is never disabled.
	count = maxIdleScheduledRuns - 1
	if count, _ = nextIdleCount(count, true); count != 0 {
		t.Fatalf("write must reset streak to 0, got %d", count)
	}
	if _, disable := nextIdleCount(count, false); disable {
		t.Fatal("one idle run after a reset must not disable")
	}
}

func taskIdleCount(t *testing.T, pdb *db.ProjectDB, taskID int64) int {
	t.Helper()
	var n int
	if err := pdb.QueryRow(
		"SELECT COALESCE(idle_run_count, 0) FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&n); err != nil {
		t.Fatalf("read idle_run_count: %v", err)
	}
	return n
}

func taskEnabled(t *testing.T, pdb *db.ProjectDB, taskID int64) bool {
	t.Helper()
	var enabled bool
	if err := pdb.QueryRow(
		"SELECT is_enabled FROM of_scheduled_tasks WHERE id = ?", taskID).Scan(&enabled); err != nil {
		t.Fatalf("read is_enabled: %v", err)
	}
	return enabled
}

// TestTrackScheduledIdlePersists verifies the idle counter is stored on the
// task row (and so survives a restart), not held in process memory.
func TestTrackScheduledIdlePersists(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	taskID, _ := seedScheduledTask(t, pdb, "do a thing", "")

	// Each idle run bumps the persisted counter. Reading it back from the DB
	// proves the count is sourced from the row, not an in-memory map that a
	// process restart would clear.
	for want := 1; want <= 3; want++ {
		w.trackScheduledIdle(SessionResult{Writes: 0}, int(taskID))
		if got := taskIdleCount(t, pdb, taskID); got != want {
			t.Fatalf("after %d idle runs idle_run_count = %d, want %d", want, got, want)
		}
	}

	// A run that wrote something resets the streak to 0.
	w.trackScheduledIdle(SessionResult{Writes: 1}, int(taskID))
	if got := taskIdleCount(t, pdb, taskID); got != 0 {
		t.Fatalf("a writing run must reset idle_run_count to 0, got %d", got)
	}

	// Driving a pure heartbeat to the threshold disables the task and resets the
	// counter so a later re-enable starts fresh.
	for i := 0; i < maxIdleScheduledRuns; i++ {
		w.trackScheduledIdle(SessionResult{Writes: 0}, int(taskID))
	}
	if taskEnabled(t, pdb, taskID) {
		t.Fatal("task must be disabled after maxIdleScheduledRuns idle runs")
	}
	if got := taskIdleCount(t, pdb, taskID); got != 0 {
		t.Fatalf("idle_run_count must reset to 0 on disable, got %d", got)
	}
}
