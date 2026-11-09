package engine

// The stage drive loop: resolve/create the run, walk the pipeline stage by
// stage with panic recovery, per-stage timeouts, and the circuit breaker. Split
// out of worker.go; the command loop that re-enters drive() stays there.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"openfabrica/internal/buildstate"
)

// circuit-breaker tuning for the stage drive loop.
const (
	maxStageErrors    = 3
	stageRetryBackoff = 20 * time.Second
)

// stageRetryBase is the per-retry backoff unit (attempt N waits N × base,
// capped at stageRetryBackoff). A var so tests can shrink the wait.
var stageRetryBase = 5 * time.Second

// errStagePaused wraps ErrPaused with run-loop semantics: the drive loop
// stops cleanly without counting an error.
var errStagePaused = errors.New("stage paused")

// stageTimeout returns the wall-clock cap for a stage, preferring a configured
// override (Deps.StageTimeouts) over the stage's own StageInfo.Timeout.
func (w *Worker) stageTimeout(stage string) time.Duration {
	if w.deps != nil {
		if t, ok := w.deps.StageTimeouts[stage]; ok && t > 0 {
			return t
		}
	}
	return stageInfo(stage).Timeout
}

// drive resolves the project's run and walks the stage pipeline until done,
// paused, or failed. Re-entrant: a resumed run continues from its stored
// stage; done/blocked tasks make re-running idempotent.
func (w *Worker) drive(ctx context.Context) {
	// A request nobody consumed (a stop that raced an idle worker) must not kill
	// the next build: entering drive is a fresh intent to run.
	w.clearStopRequest()

	run, fresh, err := w.resolveRun()
	if err != nil {
		w.logger.Error("resolve run failed", "error", err)
		w.publishError(fmt.Sprintf("I couldn't pick up where we left off just now. %v. Give me a nudge to try again.", err))
		w.setState(StateError)
		return
	}
	w.run = run

	if proceed, waiting := w.autoRecover(run); !proceed {
		// Waiting on the owner is not a manual pause: leave projects.mode alone so
		// the answer's wake resumes the run (a sticky mode=paused would swallow it).
		if !waiting {
			w.markModePaused()
		}
		w.setState(StatePaused)
		return
	}

	// Auto-named projects get a real, description-derived title before any
	// stage runs, so the owner sees it the moment the build kicks off.
	// Best-effort and fresh-build-only (a resume must not re-spend the call).
	if fresh && run.Kind == RunKindBuild {
		w.maybeGenerateTitle(ctx)
	}

	stage := run.Stage
	prevStage := ""
	prevStageDurationMs := int64(0)
	stageRetries := 0
	for {
		if ctx.Err() != nil {
			return
		}

		w.publishStageChange(prevStage, stage, prevStageDurationMs)
		if line := w.stageEntryLine(stage); line != "" {
			w.publishMessage(line)
		}
		w.runlog.stage(run.ID, stage, buildstate.StageStarted, "", 0, 0, 0)

		start := time.Now()
		outcome, stageErr := w.runStageSafely(ctx, stage, run)
		durationMs := time.Since(start).Milliseconds()

		if w.settleOwnerStop(run, stage, outcome, stageErr, durationMs) {
			return
		}

		switch {
		case errors.Is(stageErr, errStagePaused):
			// A pause settles the wake payload: the next resume refolds the
			// full answer set from the DB (autoRecover / handleWakeCommand).
			w.clearWakeAnswer()
			w.runlog.stage(run.ID, stage, buildstate.StagePaused, "", 0, 0, durationMs)
			// pauseAwaitingAnswers already set run + state.
			return

		case stageErr != nil:
			stageRetries++
			if !w.retryAfterStageError(ctx, run, stage, stageErr, stageRetries, durationMs) {
				return
			}
			// The wake payload is deliberately NOT cleared here: the retried
			// stage re-reads (peekWakeAnswer) the owner answer it was woken
			// with, instead of re-planning/re-building answerless.
			continue // retry same stage

		case outcome.Done && outcome.Blocked:
			// Must sit ahead of the Done branch, which stamps stage=live/
			// status=completed on EVERY finished build.
			w.endBlocked(run, stage, durationMs)
			return

		case outcome.Done:
			w.endCompleted(run, stage, durationMs)
			return

		default:
			next, ok := w.advanceStage(run, stage, outcome, durationMs)
			if !ok {
				return
			}
			prevStage = stage
			prevStageDurationMs = durationMs
			stage = next
			stageRetries = 0
		}
	}
}

// settleOwnerStop lands an owner stop that arrived while the stage ran, reporting
// whether it did (the drive loop then returns). It settles ahead of every other
// outcome so a stop lands the same way whatever the interrupted stage returned (a
// stopped stage may report an error or an owner-question pause, and the error path
// would also count a circuit-breaker strike). A stage that FINISHED before the stop
// landed is exempt: the work is done. The request is consumed either way, here
// rather than per-branch: left set it would survive into the live project and kill
// the first scheduled task to run there.
func (w *Worker) settleOwnerStop(run *Run, stage string, outcome Outcome, stageErr error, durationMs int64) bool {
	stopped := w.stopRequested()
	if stopped {
		w.clearStopRequest()
	}
	if !stopped || (stageErr == nil && outcome.Done) {
		return false
	}
	w.clearWakeAnswer()
	// A completed intermediate stage keeps what it earned: advance the
	// pointer so the resume starts at the next stage rather than paying
	// for the finished one over again.
	if next := resolveNext(stage, outcome); stageErr == nil && next != "" {
		w.runlog.stage(run.ID, stage, buildstate.StageCompleted, "", 0, 0, durationMs)
		if err := run.setStage(w.pdb, next); err != nil {
			w.logger.Warn("advance stage on owner stop failed", "error", err)
		}
	} else {
		w.runlog.stage(run.ID, stage, buildstate.StageStopped, "", 0, 0, durationMs)
	}
	w.pauseOwnerStop()
	w.publishMessage("Stopped there. Nothing's lost: set the mode back to Building whenever you want me to pick up where I left off.")
	// Announced last, once the run is really parked: the chat releases its
	// "Stopping…" hold on this, and releasing it before the park would put a
	// live Stop button back in front of an owner whose stop already won.
	w.publishStopLanded("build")
	return true
}

// retryAfterStageError applies the circuit breaker to a failed stage attempt
// (attempt counts this one) and reports whether to run the stage again. A
// permanent error pauses at once; a transient one is recorded and retried after
// a growing backoff until maxStageErrors, then the run fails and pauses.
func (w *Worker) retryAfterStageError(ctx context.Context, run *Run, stage string, stageErr error, attempt int, durationMs int64) bool {
	if ctx.Err() != nil {
		return false
	}
	w.logger.Error("stage failed", "stage", stage, "error", stageErr)
	w.runlog.stage(run.ID, stage, buildstate.StageFailed, stageErr.Error(), 0, 0, durationMs)

	// A stage that hit its wall-clock cap would hit it again: retrying would spend up to
	// maxStageErrors more full stage budgets on the same slow work. Park it for the owner.
	if errors.Is(stageErr, context.DeadlineExceeded) {
		w.clearWakeAnswer()
		w.pauseForFailure(run, fmt.Sprintf("stage %s ran out of time", stage),
			fmt.Sprintf("I hit a snag during %s, so I paused to keep everything safe. %v. Once that's sorted I'll pick back up.", stage, stageErr), true)
		return false
	}

	// Circuit breaker: permanent errors pause immediately.
	if isPermanentError(stageErr) {
		w.clearWakeAnswer()
		if msg, ok := quotaPauseMessage(stageErr); ok {
			w.pauseForFailure(run, fmt.Sprintf("provider quota exhausted in %s", stage), msg, false)
		} else if msg, ok := budgetPauseMessage(stageErr); ok {
			w.pauseForFailure(run, fmt.Sprintf("daily token budget exhausted in %s", stage), msg, false)
		} else {
			w.pauseForFailure(run, fmt.Sprintf("permanent error in %s: %v", stage, stageErr),
				fmt.Sprintf("I hit a snag during %s, so I paused to keep everything safe. %v. Once that's sorted I'll pick back up.", stage, stageErr), true)
		}
		return false
	}

	if _, rerr := run.recordError(w.pdb, stageErr.Error()); rerr != nil {
		w.logger.Warn("record run error failed", "error", rerr)
	}
	if attempt >= maxStageErrors {
		w.clearWakeAnswer()
		// Paused, not failed: a failed run is never resumed, so the owner's resume would
		// start over at EXPAND and PLAN and pay for them again.
		if err := run.pause(w.pdb, fmt.Sprintf("stage %s kept failing: %v", stage, stageErr)); err != nil {
			w.logger.Warn("pause run failed", "error", err)
		}
		w.markModePaused()
		w.publishError(fmt.Sprintf("I kept running into trouble during %s (%d tries) and paused so nothing breaks. Latest issue: %v. Let me know and we'll sort it together.", stage, attempt, stageErr))
		w.setState(StatePaused)
		return false
	}
	backoff := time.Duration(attempt) * stageRetryBase
	if backoff > stageRetryBackoff {
		backoff = stageRetryBackoff
	}
	w.publishMessage(fmt.Sprintf("That didn't go through the first time (try %d of %d). Giving it another go now.", attempt, maxStageErrors))
	select {
	case <-ctx.Done():
		return false
	case <-time.After(backoff):
	}
	return true
}

// endBlocked ends a build that finished but produced something that must not
// reach a user (engine/blocking.go). The run ends failed at the stage it stopped
// in, so the honest state is what the owner, the admin UI, and the triage scan
// all read.
func (w *Worker) endBlocked(run *Run, stage string, durationMs int64) {
	w.clearWakeAnswer()
	w.runlog.stage(run.ID, stage, buildstate.StageBlocked, "", 0, 0, durationMs)
	if err := run.fail(w.pdb, "blocking issues prevented go-live"); err != nil {
		w.logger.Warn("fail blocked run failed", "error", err)
	}
	w.runlog.summary(run.ID)
	// Mirror the stop in projects.mode: left on building, every process start would
	// run the whole pipeline again and rebuild the same blocked project.
	w.markModePaused()
	w.setState(StateError)
}

// endCompleted closes a finished run: stage=live, status=completed.
func (w *Worker) endCompleted(run *Run, stage string, durationMs int64) {
	w.clearWakeAnswer()
	w.runlog.stage(run.ID, stage, buildstate.StageCompleted, "", 0, 0, durationMs)
	if err := run.setStage(w.pdb, StageLive); err != nil {
		w.logger.Warn("set terminal stage failed", "error", err)
	}
	if err := run.complete(w.pdb); err != nil {
		w.logger.Warn("complete run failed", "error", err)
	}
	w.runlog.summary(run.ID)
	// goLive already ran inside the stage (completeBuild); ensure state.
	w.setState(StateLive)
}

// advanceStage records a finished intermediate stage and moves the run's stage
// pointer to the next one, returning it. False means the run was parked instead
// (no next stage, or the pointer could not be persisted).
func (w *Worker) advanceStage(run *Run, stage string, outcome Outcome, durationMs int64) (string, bool) {
	next := resolveNext(stage, outcome)
	w.clearWakeAnswer()
	w.runlog.stage(run.ID, stage, buildstate.StageCompleted, "", 0, 0, durationMs)
	if next == "" {
		w.pauseForFailure(run, fmt.Sprintf("stage %s finished without naming a next stage", stage),
			fmt.Sprintf("I finished %s but there was no next step to move on to, so I paused to keep everything safe.", stage), true)
		return "", false
	}
	if err := run.setStage(w.pdb, next); err != nil {
		w.pauseForFailure(run, fmt.Sprintf("stage advance from %s to %s failed: %v", stage, next, err),
			fmt.Sprintf("I finished %s but couldn't move on to %s, so I paused to keep everything safe. %v.", stage, next, err), true)
		return "", false
	}
	// A stage that finished clears the strikes earlier stages collected.
	if run.ErrorCount > 0 {
		if err := run.resetErrors(w.pdb); err != nil {
			w.logger.Warn("reset run error count failed", "error", err)
		}
	}
	return next, true
}

// pauseForFailure parks a run the drive loop cannot continue on its own: the run
// row records reason, projects.mode mirrors the pause so no wake resumes it
// (only the owner flipping back to building does), the owner hears ownerMsg (as
// an error when asError), and the worker settles paused.
func (w *Worker) pauseForFailure(run *Run, reason, ownerMsg string, asError bool) {
	if err := run.pause(w.pdb, reason); err != nil {
		w.logger.Warn("pause run failed", "error", err)
	}
	w.markModePaused()
	if asError {
		w.publishError(ownerMsg)
	} else {
		w.publishMessage(ownerMsg)
	}
	w.setState(StatePaused)
}

// runStageSafely invokes a stage with panic recovery and the stage timeout.
func (w *Worker) runStageSafely(ctx context.Context, stage string, run *Run) (outcome Outcome, stageErr error) {
	defer func() {
		if r := recover(); r != nil {
			stageErr = fmt.Errorf("panic in stage %s: %v", stage, r)
			w.logger.Error("stage panic", "stage", stage, "panic", r)
		}
	}()
	s, err := stageByName(stage)
	if err != nil {
		return Outcome{}, err
	}
	stageCtx := ctx
	if timeout := w.stageTimeout(stage); timeout > 0 {
		var cancel context.CancelFunc
		stageCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return s.Run(stageCtx, w, run)
}

// resolveRun returns the active run (resuming it) or creates a fresh build
// run, reporting which happened. Fresh runs enter at EXPAND when the
// project's ambition level enables it, else directly at PLAN.
func (w *Worker) resolveRun() (run *Run, fresh bool, err error) {
	run, err = activeRun(w.pdb)
	if err != nil {
		return nil, false, err
	}
	if run != nil {
		return run, false, nil
	}
	entry := firstStage()
	if entry == StageExpand && w.projectAmbitionLevel() == AmbitionOff {
		entry = StagePlan
	}
	run, err = createRun(w.pdb, RunKindBuild, entry, "")
	return run, true, err
}

// autoRecover decides whether a (resumed) run may proceed. proceed=false means
// stay paused; waiting reports that the pause is only the owner not having
// answered yet (their answer wakes the worker), as opposed to a failure pause
// that needs an explicit owner action.
func (w *Worker) autoRecover(run *Run) (proceed, waiting bool) {
	// A run paused by a failure is only driven again when the owner flipped the mode
	// back to building (a failure pause mirrors into projects.mode), which is the
	// explicit resume: it earns a fresh set of strikes. A run still marked running with
	// a full count is a crash loop and stays refused below.
	if run.Status == RunStatusPaused && run.ErrorCount > 0 &&
		run.PauseReason != PauseReasonOwnerAnswers && run.PauseReason != PauseReasonApproval {
		if err := run.resetErrors(w.pdb); err != nil {
			w.logger.Warn("reset run error count failed", "error", err)
		}
	}
	if run.ErrorCount >= maxStageErrors {
		w.logger.Warn("auto-recovery skipped: error count too high", "errors", run.ErrorCount)
		w.publishMessage(fmt.Sprintf(
			"I ran into a few too many bumps (%d) and paused to play it safe. Tap resume whenever you'd like me to try again.", run.ErrorCount))
		return false, false
	}
	if run.Status == RunStatusPaused {
		switch run.PauseReason {
		case PauseReasonOwnerAnswers, PauseReasonApproval:
			if w.pendingQuestionCount() > 0 {
				if run.PauseReason == PauseReasonApproval {
					w.publishMessage("We're paused waiting for your approval.")
				} else {
					w.publishMessage("We're paused for the moment: waiting on your answers above.")
				}
				return false, true
			}
			// All answered: fold the full answer set in and resume.
			if answers := w.collectAnswers(); answers != "" {
				w.wakeContext = map[string]any{"reason": "question_answered", "answer": answers}
			}
			if err := run.resume(w.pdb); err != nil {
				w.logger.Warn("resume run failed", "error", err)
			}
		default:
			// Failure pauses recover on an explicit resume (this code path runs
			// because the owner flipped the mode back to building).
			if err := run.resume(w.pdb); err != nil {
				w.logger.Warn("resume run failed", "error", err)
			}
		}
		w.publishPhase()
	}
	return true, false
}
