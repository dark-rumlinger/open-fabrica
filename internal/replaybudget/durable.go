package replaybudget

// The durable budget keeps one of_flow_budget row per run, so the replay cap and
// stall state outlive the process. A single run is a build session then a repair
// session, and crash-resume can restart it mid-run; the in-memory counters this
// replaces reset on every such restart, which let a build replay far past Max (see
// docs/TRIAGE.md). Keying on the run id, read from the DB each time, bounds the
// whole run instead.
//
// Every operation fails open: a DB error lets the replay through and reports no
// stall, because bookkeeping must never wedge a build. Project DBs carry the table
// from the initial schema (db/project_migrations/001_initial.sql); a missing table
// only happens in tests that bypass migrations, and those use the in-memory path.

import (
	"database/sql"

	"openfabrica/internal/dbh"
)

// budgetRow is one run's replay count and stall state. best is -1 before the first
// red full run, matching stallState.
type budgetRow struct {
	best         int
	bestDepth    int
	partialDepth int
	stallCount   int
	red          bool
}

// takeDurable records one replay against the run and reports whether it was within
// budget. The read and the increment share one write-pool transaction so a
// concurrent caller cannot drive the count past Max.
func takeDurable(db dbh.Handle, runID string) bool {
	tx, err := db.Begin()
	if err != nil {
		return true
	}
	defer tx.Rollback()
	var replays int
	err = tx.QueryRow(`SELECT replays FROM of_flow_budget WHERE run_id = ?`, runID).Scan(&replays)
	if err != nil && err != sql.ErrNoRows {
		return true
	}
	if replays >= Max {
		return false
	}
	if _, err := tx.Exec(
		`INSERT INTO of_flow_budget(run_id, replays) VALUES(?, 1)
		 ON CONFLICT(run_id) DO UPDATE SET replays = replays + 1, updated_at = CURRENT_TIMESTAMP`,
		runID); err != nil {
		return true
	}
	if err := tx.Commit(); err != nil {
		return true
	}
	return true
}

// leftDurable is how many replays the run may still take. No row or a read error
// means the full budget.
func leftDurable(db dbh.Handle, runID string) int {
	var replays int
	if err := db.QueryRow(`SELECT replays FROM of_flow_budget WHERE run_id = ?`, runID).Scan(&replays); err != nil {
		return Max
	}
	if left := Max - replays; left > 0 {
		return left
	}
	return 0
}

// stalledDurable reports whether the run is at the stall cap.
func stalledDurable(db dbh.Handle, runID string) bool {
	var n int
	if err := db.QueryRow(`SELECT stall_count FROM of_flow_budget WHERE run_id = ?`, runID).Scan(&n); err != nil {
		return false
	}
	return n >= MaxStalls
}

// lastRedDurable reports whether the run's latest replay failed.
func lastRedDurable(db dbh.Handle, runID string) bool {
	var red int
	if err := db.QueryRow(`SELECT red FROM of_flow_budget WHERE run_id = ?`, runID).Scan(&red); err != nil {
		return false
	}
	return red != 0
}

// observeDurable folds one replay's outcome into the run's stall state and returns
// the replays in a row without progress. It never touches replays, so a green does
// not refund the budget.
func observeDurable(db dbh.Handle, runID string, failed, depth int, full bool) int {
	tx, err := db.Begin()
	if err != nil {
		return 0
	}
	defer tx.Rollback()
	r := budgetRow{best: -1}
	var red int
	err = tx.QueryRow(
		`SELECT best, best_depth, partial_depth, stall_count, red FROM of_flow_budget WHERE run_id = ?`,
		runID).Scan(&r.best, &r.bestDepth, &r.partialDepth, &r.stallCount, &red)
	switch {
	case err == sql.ErrNoRows:
		r = budgetRow{best: -1}
	case err != nil:
		return 0
	default:
		r.red = red != 0
	}
	applyObserve(&r, failed, depth, full)
	redInt := 0
	if r.red {
		redInt = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO of_flow_budget(run_id, best, best_depth, partial_depth, stall_count, red)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET
		    best = excluded.best, best_depth = excluded.best_depth,
		    partial_depth = excluded.partial_depth, stall_count = excluded.stall_count,
		    red = excluded.red, updated_at = CURRENT_TIMESTAMP`,
		runID, r.best, r.bestDepth, r.partialDepth, r.stallCount, redInt); err != nil {
		return 0
	}
	if err := tx.Commit(); err != nil {
		return 0
	}
	return r.stallCount
}

// resetStallsDurable clears the run's stall state but keeps the replays it has
// already taken.
func resetStallsDurable(db dbh.Handle, runID string) {
	_, _ = db.Exec(
		`UPDATE of_flow_budget SET best = -1, best_depth = 0, partial_depth = 0,
		    stall_count = 0, red = 0, updated_at = CURRENT_TIMESTAMP WHERE run_id = ?`,
		runID)
}

// applyObserve is the stall state machine, identical to Stalls.Observe: a green
// full run clears the stall fields, a red full run with fewer failed flows (or the
// same number failing further in) is progress, and any other red adds a stall.
func applyObserve(r *budgetRow, failed, depth int, full bool) {
	switch {
	case failed == 0 && full:
		r.best, r.bestDepth, r.partialDepth, r.stallCount, r.red = -1, 0, 0, 0, false
	case failed == 0:
		r.stallCount, r.red = 0, false
	case !full && depth > r.partialDepth:
		r.partialDepth, r.stallCount, r.red = depth, 0, true
	case !full:
		r.stallCount++
		r.red = true
	case r.best < 0:
		r.best, r.bestDepth, r.red, r.partialDepth = failed, depth, true, 0
	case failed < r.best || (failed == r.best && depth > r.bestDepth):
		r.best, r.bestDepth, r.stallCount, r.red, r.partialDepth = failed, depth, 0, true, 0
	default:
		r.stallCount++
		r.red = true
	}
}
