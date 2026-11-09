package jobs

import (
	"errors"
	"log/slog"
	"testing"

	"openfabrica/internal/dbtest"
)

// A failing job must be rescheduled (attempts left) or marked failed (none left),
// with its error recorded. Both updates write of_jobs.error, which the migrated schema
// must provide, or a failure cannot be recorded and the job wedges in 'running'. Runs
// against the real migrated schema.
func TestHandleFailureRecordsErrorAndStatus(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	w := &Worker{projectID: 1, projectDB: pdb, logger: slog.New(slog.DiscardHandler)}

	insert := func(attempts, max int) int64 {
		res, err := pdb.ExecWrite(
			`INSERT INTO of_jobs (type, payload, status, attempts, max_attempts) VALUES ('http_request', '{}', 'running', ?, ?)`,
			attempts, max)
		if err != nil {
			t.Fatalf("insert job: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	read := func(id int64) (status, errText string) {
		if err := pdb.Writer().QueryRow(`SELECT status, error FROM of_jobs WHERE id = ?`, id).Scan(&status, &errText); err != nil {
			t.Fatalf("read job: %v", err)
		}
		return
	}

	retry := insert(1, 3)
	w.handleFailure(retry, errors.New("upstream 503"))
	if status, e := read(retry); status != "pending" || e != "upstream 503" {
		t.Fatalf("retryable job: status=%q error=%q, want pending with the error recorded", status, e)
	}

	final := insert(3, 3)
	w.handleFailure(final, errors.New("gave up"))
	if status, e := read(final); status != "failed" || e != "gave up" {
		t.Fatalf("exhausted job: status=%q error=%q, want failed with the error recorded", status, e)
	}
}

// The llm_prompt result column the worker writes exists too.
func TestJobsResultColumnExists(t *testing.T) {
	pdb := dbtest.ProjectWriterDB(t)
	res, err := pdb.ExecWrite(`INSERT INTO of_jobs (type, payload) VALUES ('llm_prompt', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := pdb.ExecWrite(`UPDATE of_jobs SET result = ? WHERE id = ?`, "done", id); err != nil {
		t.Fatalf("of_jobs.result must exist: %v", err)
	}
}
