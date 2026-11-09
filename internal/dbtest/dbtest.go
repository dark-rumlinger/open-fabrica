// Package dbtest provides helpers for exercising handlers against a real
// per-project database. Production's writer pool is capped at one connection, so
// a handler issuing a second query while a cursor is still open hangs there while
// an uncapped in-memory *sql.DB would pass; these helpers use the real pool so
// that class of defect fails the suite instead of only production.
package dbtest

import (
	"testing"
	"time"

	"openfabrica/db"
)

// ProjectWriterDB opens a real per-project database, migrations included, so its
// writer pool carries the production single-connection cap.
func ProjectWriterDB(t *testing.T) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	return pdb
}

// RunWithDeadlineGuard runs fn in a goroutine and fails the test if it does not
// return within d, turning a writer-pool self-deadlock into a fast, clear
// failure. fn is left running on timeout: a handler wedged on the writer pool
// cannot be cancelled from outside.
func RunWithDeadlineGuard(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s: likely a writer-pool deadlock", name, d)
	}
}
