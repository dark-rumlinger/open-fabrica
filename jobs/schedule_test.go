package jobs

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"openfabrica/internal/dbtest"
)

func newScheduleWorker(t *testing.T) *Worker {
	t.Helper()
	pdb := dbtest.ProjectWriterDB(t)
	return &Worker{projectID: 1, projectDB: pdb, logger: slog.New(slog.DiscardHandler)}
}

func (w *Worker) addJob(t *testing.T, jobType, status, scheduledAt string, attempts, max int) int64 {
	t.Helper()
	res, err := w.projectDB.ExecWrite(
		`INSERT INTO of_jobs (type, payload, status, scheduled_at, attempts, max_attempts) VALUES (?, '{}', ?, ?, ?, ?)`,
		jobType, status, scheduledAt, attempts, max)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (w *Worker) jobStatus(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := w.projectDB.Writer().QueryRow(`SELECT status FROM of_jobs WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// An ISO scheduled_at must be compared as a time: a past ISO value is due now, a
// future one waits, and offsets are honored. Compared as text, "T" sorts after
// the space in the stored format and a same-day job would wait until tomorrow.
func TestProcessPendingHonorsISOScheduledAt(t *testing.T) {
	w := newScheduleWorker(t)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	// 30 minutes ago in UTC, written with a +05:00 offset: due.
	offsetPast := time.Now().UTC().Add(-30 * time.Minute).In(time.FixedZone("x", 5*3600)).Format(time.RFC3339)

	due := w.addJob(t, "unknown_type", "pending", past, 0, 1)
	later := w.addJob(t, "unknown_type", "pending", future, 0, 1)
	offset := w.addJob(t, "unknown_type", "pending", offsetPast, 0, 1)

	for w.processPending(context.Background()) {
	}
	if s := w.jobStatus(t, due); s == "pending" {
		t.Errorf("past ISO job was not picked up")
	}
	if s := w.jobStatus(t, offset); s == "pending" {
		t.Errorf("past job with a UTC offset was not picked up")
	}
	if s := w.jobStatus(t, later); s != "pending" {
		t.Errorf("future ISO job ran early, status %q", s)
	}
}

func TestProcessPendingDrainsABacklog(t *testing.T) {
	w := newScheduleWorker(t)
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, w.addJob(t, "unknown_type", "pending", "2000-01-01 00:00:00", 0, 1))
	}
	n := 0
	for w.processPending(context.Background()) {
		n++
	}
	if n != 5 {
		t.Fatalf("processed %d jobs, want 5", n)
	}
	for _, id := range ids {
		if s := w.jobStatus(t, id); s == "pending" {
			t.Errorf("job %d left pending", id)
		}
	}
}

func TestRecoverInterruptedSettlesRunningJobs(t *testing.T) {
	w := newScheduleWorker(t)
	retry := w.addJob(t, "send_email", "running", "2000-01-01 00:00:00", 1, 3)
	spent := w.addJob(t, "send_email", "running", "2000-01-01 00:00:00", 3, 3)
	done := w.addJob(t, "send_email", "completed", "2000-01-01 00:00:00", 1, 3)

	w.recoverInterrupted()
	if s := w.jobStatus(t, retry); s != "pending" {
		t.Errorf("interrupted job with attempts left: %q, want pending", s)
	}
	if s := w.jobStatus(t, spent); s != "failed" {
		t.Errorf("interrupted job out of attempts: %q, want failed", s)
	}
	if s := w.jobStatus(t, done); s != "completed" {
		t.Errorf("completed job disturbed: %q", s)
	}
}

func TestCompletionDoesNotOverwriteCancel(t *testing.T) {
	w := newScheduleWorker(t)
	id := w.addJob(t, "send_email", "cancelled", "2000-01-01 00:00:00", 1, 3)
	if _, err := w.projectDB.ExecWrite("UPDATE of_jobs SET status = 'completed' WHERE id = ? AND status = 'running'", id); err != nil {
		t.Fatal(err)
	}
	w.handleFailure(id, context.DeadlineExceeded)
	if s := w.jobStatus(t, id); s != "cancelled" {
		t.Errorf("cancelled job overwritten: %q", s)
	}
}
