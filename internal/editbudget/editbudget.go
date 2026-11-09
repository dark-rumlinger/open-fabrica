// Package editbudget keeps patch-failure counts for a build in the project DB
// instead of process memory, so a stuck edit loop is caught across the whole run.
//
// A run is a build session then a repair session, and a segment rollover or a
// crash-resume restarts the session mid-run. The in-memory counters this backs
// (tools.ReadTracker's edit-failure maps) reset on every such restart, so a model
// re-sending the same no-op patch across the boundary never reached the STOP that
// was meant to break it (see docs/TRIAGE.md, the same bug the replay budget had).
// Keying on the run id, read from the DB each time, bounds the whole run.
//
// Every operation fails open: a DB error returns a count of 0 (no escalation) and
// a clear is best-effort, because bookkeeping must never wedge a build. Project DBs
// carry of_edit_failures from the initial schema; a missing table only happens in
// tests that bypass migrations, which use the in-memory path.
package editbudget

import "openfabrica/internal/dbh"

// Categories separate the failure shapes that escalate differently. Anchor covers
// every patch that could not be applied to the file (anchor not found, consumed by
// an earlier hunk, chained, or a refused fuzzy deletion): a successful write clears
// it, so the count is a streak that survives a rollover. NoOp is a patch whose
// replace already equals search; it is never cleared, so its count is the run total
// of wasted no-change patches.
const (
	Anchor = "anchor"
	NoOp   = "noop"
	// Nudge is not a failure category: it is a per-run one-shot flag store. A caller
	// records a nudge key and acts only when Record returns 1, so a reminder fires at
	// most once per run. It lives here to reuse the same durable per-run KV table.
	Nudge = "nudge"
)

// Record increments the count for (run, category, key) and returns the new value.
// Returns 0 on any DB error so a bookkeeping failure never triggers an escalation.
func Record(db dbh.Handle, runID, category, key string) int {
	if db == nil || runID == "" {
		return 0
	}
	tx, err := db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO of_edit_failures(run_id, category, key, count) VALUES(?, ?, ?, 1)
		 ON CONFLICT(run_id, category, key) DO UPDATE SET count = count + 1, updated_at = CURRENT_TIMESTAMP`,
		runID, category, key); err != nil {
		return 0
	}
	var n int
	if err := tx.QueryRow(
		`SELECT count FROM of_edit_failures WHERE run_id = ? AND category = ? AND key = ?`,
		runID, category, key).Scan(&n); err != nil {
		return 0
	}
	if err := tx.Commit(); err != nil {
		return 0
	}
	return n
}

// RunTotal is the sum of a category's counts across every key in the run, so a
// model spreading the same mistake over many files is caught even when no single
// file reaches the per-file cap. 0 on any DB error.
func RunTotal(db dbh.Handle, runID, category string) int {
	if db == nil || runID == "" {
		return 0
	}
	var n int
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(count), 0) FROM of_edit_failures WHERE run_id = ? AND category = ?`,
		runID, category).Scan(&n); err != nil {
		return 0
	}
	return n
}

// Clear removes the count for (run, category, key), called on a successful write so
// a resolved streak does not keep escalating. Best-effort: a failure is ignored.
func Clear(db dbh.Handle, runID, category, key string) {
	if db == nil || runID == "" {
		return
	}
	_, _ = db.Exec(
		`DELETE FROM of_edit_failures WHERE run_id = ? AND category = ? AND key = ?`,
		runID, category, key)
}
