package engine

// of_runs I/O. A Run is one pass through the stage pipeline: a full build
// (kind=build) or a LIVE update cycle (kind=update). The latest row carries
// the project's current plan and stage; an active run is the single row with
// status running or paused.

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"openfabrica/db"
	"openfabrica/internal/buildstate"
	"openfabrica/internal/sqliteretry"
)

// Run statuses (defined in internal/buildstate, shared with the admin views).
const (
	RunStatusRunning   = buildstate.RunRunning
	RunStatusPaused    = buildstate.RunPaused
	RunStatusCompleted = buildstate.RunCompleted
	RunStatusFailed    = buildstate.RunFailed
)

// Run kinds.
const (
	RunKindBuild  = "build"
	RunKindUpdate = "update"
)

// Run is one pipeline run.
type Run struct {
	ID                string
	Kind              string
	Status            string
	Stage             string
	PlanJSON          string
	UpdateDescription string
	ExpandedBrief     string
	PauseReason       string
	ErrorCount        int
	LastError         string

	TotalInputTokens      int64
	TotalOutputTokens     int64
	TotalCacheReadTokens  int64
	TotalCacheWriteTokens int64
}

const runColumns = `id, kind, status, stage, COALESCE(plan_json,''), COALESCE(update_description,''),
	expanded_brief, COALESCE(pause_reason,''), error_count, COALESCE(last_error,''),
	total_input_tokens, total_output_tokens, total_cache_read_tokens, total_cache_write_tokens`

func scanRun(row *sql.Row) (*Run, error) {
	r := &Run{}
	err := row.Scan(&r.ID, &r.Kind, &r.Status, &r.Stage, &r.PlanJSON, &r.UpdateDescription,
		&r.ExpandedBrief, &r.PauseReason, &r.ErrorCount, &r.LastError,
		&r.TotalInputTokens, &r.TotalOutputTokens, &r.TotalCacheReadTokens, &r.TotalCacheWriteTokens)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// runSeq disambiguates run ids minted within the same millisecond (a fresh
// update run right after a completed build, or back-to-back runs in tests).
var runSeq atomic.Uint64

// BuildVersion and BuildBuiltAt identify the engine binary that creates runs.
// main sets them at startup from its ldflags-injected Version / BuildTime, so each
// run row records which binary produced it and a post-mortem reads it from the run
// export instead of diffing binary mtimes against source. Empty in tests and dev
// builds, which is fine - the columns default to ”.
var (
	BuildVersion string
	BuildBuiltAt string
)

// newRunID mints a process-unique run id from the wall clock + a sequence.
func newRunID() string {
	return fmt.Sprintf("run_%d_%d", time.Now().UnixMilli(), runSeq.Add(1))
}

// createRun inserts a fresh run starting at the given stage.
func createRun(pdb *db.ProjectDB, kind, stage, updateDescription string) (*Run, error) {
	r := &Run{
		ID:                newRunID(),
		Kind:              kind,
		Status:            RunStatusRunning,
		Stage:             stage,
		UpdateDescription: updateDescription,
	}
	_, err := sqliteretry.Exec(pdb.Writer(),
		`INSERT INTO of_runs (id, kind, status, stage, update_description, engine_version, engine_built_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Kind, r.Status, r.Stage, nullIfEmptyStr(r.UpdateDescription), BuildVersion, BuildBuiltAt)
	if err != nil {
		return nil, fmt.Errorf("creating run: %w", err)
	}
	return r, nil
}

// activeRun returns the most recent running/paused run, or nil when the
// project has no run in flight.
func activeRun(pdb *db.ProjectDB) (*Run, error) {
	r, err := scanRun(pdb.QueryRow(
		`SELECT `+runColumns+` FROM of_runs
		 WHERE status IN (?, ?) ORDER BY started_at DESC, id DESC LIMIT 1`,
		RunStatusRunning, RunStatusPaused))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// latestRun returns the most recent run of any status, or nil when the
// project has never run.
func latestRun(pdb *db.ProjectDB) (*Run, error) {
	r, err := scanRun(pdb.QueryRow(
		`SELECT ` + runColumns + ` FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1`))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// latestCompletedPlan returns the plan_json of the most recent run that has
// one - the project's current plan regardless of run outcome.
func latestCompletedPlan(pdb *db.ProjectDB) (string, error) {
	var planJSON string
	err := pdb.QueryRow(
		`SELECT plan_json FROM of_runs
		 WHERE plan_json IS NOT NULL AND plan_json != ''
		 ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&planJSON)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return planJSON, err
}

func (r *Run) update(pdb *db.ProjectDB, set string, args ...any) error {
	args = append(args, r.ID)
	_, err := sqliteretry.Exec(pdb.Writer(),
		`UPDATE of_runs SET `+set+`, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, args...)
	return err
}

// setStage advances the run to a new stage.
func (r *Run) setStage(pdb *db.ProjectDB, stage string) error {
	r.Stage = stage
	return r.update(pdb, "stage = ?", stage)
}

// setPlan stores the parsed plan JSON on the run.
func (r *Run) setPlan(pdb *db.ProjectDB, planJSON string) error {
	r.PlanJSON = planJSON
	return r.update(pdb, "plan_json = ?", planJSON)
}

// pause marks the run paused with a reason.
func (r *Run) pause(pdb *db.ProjectDB, reason string) error {
	r.Status = RunStatusPaused
	r.PauseReason = reason
	return r.update(pdb, "status = ?, pause_reason = ?", RunStatusPaused, reason)
}

// resume returns a paused run to running.
func (r *Run) resume(pdb *db.ProjectDB) error {
	r.Status = RunStatusRunning
	r.PauseReason = ""
	return r.update(pdb, "status = ?, pause_reason = NULL", RunStatusRunning)
}

// complete marks the run finished.
func (r *Run) complete(pdb *db.ProjectDB) error {
	r.Status = RunStatusCompleted
	return r.update(pdb, "status = ?, completed_at = CURRENT_TIMESTAMP", RunStatusCompleted)
}

// fail marks the run failed with a final error.
func (r *Run) fail(pdb *db.ProjectDB, lastError string) error {
	r.Status = RunStatusFailed
	r.LastError = lastError
	return r.update(pdb, "status = ?, last_error = ?, completed_at = CURRENT_TIMESTAMP",
		RunStatusFailed, lastError)
}

// recordError bumps the run's error counter and stores the message, returning
// the new count so the worker's circuit breaker can decide.
func (r *Run) recordError(pdb *db.ProjectDB, msg string) (int, error) {
	r.ErrorCount++
	r.LastError = msg
	return r.ErrorCount, r.update(pdb, "error_count = ?, last_error = ?", r.ErrorCount, msg)
}

// resetErrors clears the run's error counter: the circuit breaker counts strikes against
// the stage in flight, not the run's whole history.
func (r *Run) resetErrors(pdb *db.ProjectDB) error {
	r.ErrorCount = 0
	return r.update(pdb, "error_count = 0")
}

// addTokens accumulates token usage onto the run's totals.
func (r *Run) addTokens(pdb *db.ProjectDB, in, out, cacheRead, cacheWrite int) error {
	r.TotalInputTokens += int64(in)
	r.TotalOutputTokens += int64(out)
	r.TotalCacheReadTokens += int64(cacheRead)
	r.TotalCacheWriteTokens += int64(cacheWrite)
	return r.update(pdb,
		`total_input_tokens = total_input_tokens + ?,
		 total_output_tokens = total_output_tokens + ?,
		 total_cache_read_tokens = total_cache_read_tokens + ?,
		 total_cache_write_tokens = total_cache_write_tokens + ?`,
		in, out, cacheRead, cacheWrite)
}

// tallyStageTokens folds one session's token usage into the active run's
// totals, in one place for every stage. A failed tally is logged, not fatal -
// bookkeeping must never sink a stage. No-op when there is no active run (some
// LIVE/scheduled paths run without one).
func (w *Worker) tallyStageTokens(res SessionResult) {
	// After go-live w.run still names the finished build; LIVE chat and scheduled
	// sessions must not add their spend to its totals.
	if w.run == nil || w.run.Status == RunStatusCompleted || w.run.Status == RunStatusFailed {
		return
	}
	if err := w.run.addTokens(w.pdb, res.InputTokens, res.OutputTokens, res.CacheReadTokens, res.CacheWriteTokens); err != nil {
		w.logger.Debug("token tally failed", "error", err)
	}
}

// runSpec runs one stage session and tallies its token usage, so no caller can
// run a session and forget to account for it. Tokens are billed even on error:
// a failed call still spent what it spent. Callers handle the returned error.
func (w *Worker) runSpec(ctx context.Context, sess *session, spec SessionSpec) (SessionResult, error) {
	res, err := sess.Run(ctx, spec)
	w.tallyStageTokens(res)
	return res, err
}

func nullIfEmptyStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
