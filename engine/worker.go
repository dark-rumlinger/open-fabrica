package engine

// Worker is the per-project goroutine: it owns the project's run lifecycle,
// dispatches stages, and reacts to commands. External access goes through Send
// (channel) and State (mutex) only. Split across worker.go (lifecycle),
// worker_drive.go (stage drive loop), worker_commands.go (command handling),
// and worker_context.go (stage-support + owner/prompt context builders).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/projectphase"
	"openfabrica/internal/replylang"
	"openfabrica/providers/llm"
	"openfabrica/tools/llmredact"
)

// Worker drives one project.
type Worker struct {
	deps      *Deps
	projectID int
	pdb       *db.ProjectDB
	logger    *slog.Logger
	limiter   *llmLimiter
	dbLog     *llm.DBLLMLogger
	runlog    *runLogger

	commands chan Command
	ctx      context.Context
	cancel   context.CancelFunc

	stateMu sync.RWMutex
	state   State

	// phaseSig is the last announced phase, so an unchanged phase is not
	// announced again. Guarded by stateMu.
	phaseSig string

	// run is the active run (nil when idle/live with no run in flight).
	run *Run

	// repairSigs maps a repair item's id to the finding signatures it answers, and
	// disputed holds the signatures of explore findings the builder completed as wrong
	// ("disputed: ..."). specAudits counts the spec audits run, so a gap a re-check
	// calls new gets a key no earlier audit used. All live for one build; see repair.go.
	repairSigs map[string][]string
	disputed   map[string]bool
	specAudits int

	// wakeContext carries answer payloads from a wake command into the next
	// stage execution.
	wakeContext map[string]any

	// chatThread is the in-memory LIVE chat conversation carried across wakes.
	chatThread []llm.Message

	// ownerInbox holds owner chat messages that arrived while a build runs. A
	// command waits until drive returns, so they are kept here instead and the build
	// session takes them at its next turn boundary (takeOwnerInbox); whatever is left
	// when the build ends is answered by a LIVE wake. Guarded by inboxMu.
	inboxMu    sync.Mutex
	ownerInbox []string

	// pendingScheduledTasks dedupes CommandScheduledTask by task id: a tick
	// for a task whose previous run is still in flight is rejected with
	// ErrScheduledTaskPending instead of queueing behind it.
	pendingScheduledTasks sync.Map

	// stopReq is the owner's pending stop request. Deliberately NOT a command:
	// commands are drained only between drives, so one sent mid-build waits for
	// the build it was meant to interrupt; an atomic lands while a stage runs.
	// Read between turns and after a stage returns, so a stop settles at a clean
	// boundary rather than abandoning an in-flight tool call.
	stopReq atomic.Bool

	// sessionsInFlight counts the LLM sessions currently running on this worker.
	// State() cannot answer "is work in flight": a LIVE chat wake never leaves
	// StateLive, so an idle live project and one mid-answer look identical. This
	// is what lets RequestStop reject a stop nothing would ever consume.
	sessionsInFlight atomic.Int32

	// providerOverride, when set, substitutes provider resolution in every
	// session this worker creates. Test seam (enginetest fake provider).
	providerOverride providerSource
}

// ErrScheduledTaskPending: a scheduled_task tick was dropped because one is
// already in flight for the same task.
var ErrScheduledTaskPending = errors.New("scheduled task already pending")

// ErrWorkerBusy: a background command (wake / scheduled_task) was rejected
// because the worker's command channel is full. Callers retry on their own
// cadence; blocking would stack producers behind a stalled LLM call.
var ErrWorkerBusy = errors.New("engine worker busy")

// isBackgroundCommand reports whether a command is fire-and-forget:
// background commands drop immediately when the channel is full, interactive
// ones (chat, mode change, update, shutdown) get a short bounded wait so
// admin actions still land under load.
func isBackgroundCommand(t string) bool {
	switch t {
	case CommandWake, CommandScheduledTask:
		return true
	}
	return false
}

// backgroundCommandsDropped counts commands rejected with ErrWorkerBusy,
// surfaced via DroppedBackgroundCommands for the system-status endpoint.
var backgroundCommandsDropped atomic.Uint64

// DroppedBackgroundCommands returns the running process-wide total of
// background commands rejected because a worker's channel was full.
func DroppedBackgroundCommands() uint64 {
	return backgroundCommandsDropped.Load()
}

func newWorker(deps *Deps, projectID int, limiter *llmLimiter, parent context.Context) (*Worker, error) {
	pdb, err := deps.ProjectDBManager.Open(projectID)
	if err != nil {
		return nil, fmt.Errorf("open project db: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	logger := slog.Default().With("project_id", projectID)
	// The logger fires its hook from its own goroutine, and only after a call has
	// been queued, which happens after w is assigned below.
	var w *Worker
	onLogged := func(e llm.LLMLogEntry, rowID int64) {
		if w != nil {
			w.publishLLMCall(e, rowID)
		}
	}
	w = &Worker{
		deps:      deps,
		projectID: projectID,
		pdb:       pdb,
		logger:    logger,
		limiter:   limiter,
		dbLog:     llm.NewDBLLMLogger(pdb.Writer(), llm.WithRedactor(llmredact.New(pdb.Reader())), llm.WithOnLogged(onLogged)),
		runlog:    &runLogger{pdb: pdb, logger: logger},
		commands:  make(chan Command, 16),
		ctx:       ctx,
		cancel:    cancel,
		state:     StateIdle,
	}
	return w, nil
}

// State returns the worker's externally visible state.
func (w *Worker) State() State {
	w.stateMu.RLock()
	defer w.stateMu.RUnlock()
	return w.state
}

func (w *Worker) setState(s State) {
	w.stateMu.Lock()
	w.state = s
	w.stateMu.Unlock()
	w.publishPhase()
}

// publishPhase announces the owner-facing phase when it differs from the last
// one announced. The phase is read back from storage rather than from the
// worker state, so it matches what the admin API reports for the same project.
func (w *Worker) publishPhase() {
	if w.deps == nil || w.deps.Bus == nil || w.deps.DB == nil || w.pdb == nil {
		return
	}
	var mode string
	if err := w.deps.DB.QueryRow("SELECT mode FROM projects WHERE id = ?", w.projectID).Scan(&mode); err != nil {
		return
	}
	in := projectphase.Inputs{Mode: mode, EngineRunning: true, Pending: w.pendingQuestionCount()}
	if run, err := latestRun(w.pdb); err == nil && run != nil {
		in.RunStatus, in.PauseReason = run.Status, run.PauseReason
	}
	phase := projectphase.Derive(in)
	sig := fmt.Sprintf("%s|%s|%s|%d", mode, phase.Key, phase.Reason, phase.Pending)

	w.stateMu.Lock()
	changed := w.phaseSig != sig
	w.phaseSig = sig
	w.stateMu.Unlock()
	if !changed {
		return
	}
	w.publishEvent(events.EventProjectPhaseChanged, map[string]any{
		"project_id": w.projectID,
		"mode":       mode,
		"phase":      phase,
	})
}

// Send enqueues a command. CommandScheduledTask dedupes by task_id (returns
// ErrScheduledTaskPending when a run is already in flight). Background
// commands (wake, scheduled_task) are non-blocking and return ErrWorkerBusy
// on a full channel; interactive commands wait up to 1s. Either way no caller
// blocks for the duration of a stalled LLM call.
func (w *Worker) Send(cmd Command) error {
	// An owner message during a build goes to the inbox the running session reads
	// between turns; queued as a command it would wait for the whole build.
	if cmd.Type == CommandChat && w.State() == StateBuilding {
		if msg, _ := cmd.Payload["message"].(string); strings.TrimSpace(msg) != "" {
			w.pushOwnerInbox(msg)
			return nil
		}
	}
	var claimedTaskID int64
	if cmd.Type == CommandScheduledTask {
		taskID := payloadInt64(cmd.Payload, "task_id")
		if taskID > 0 {
			if _, loaded := w.pendingScheduledTasks.LoadOrStore(taskID, struct{}{}); loaded {
				return ErrScheduledTaskPending
			}
			claimedTaskID = taskID
		}
	}

	if isBackgroundCommand(cmd.Type) {
		select {
		case w.commands <- cmd:
			return nil
		case <-w.ctx.Done():
			if claimedTaskID > 0 {
				w.pendingScheduledTasks.Delete(claimedTaskID)
			}
			return fmt.Errorf("engine worker shutting down")
		default:
			if claimedTaskID > 0 {
				w.pendingScheduledTasks.Delete(claimedTaskID)
			}
			backgroundCommandsDropped.Add(1)
			return ErrWorkerBusy
		}
	}

	select {
	case w.commands <- cmd:
		return nil
	case <-w.ctx.Done():
		return fmt.Errorf("engine worker shutting down")
	case <-time.After(1 * time.Second):
		return fmt.Errorf("worker command channel full, command %s not delivered", cmd.Type)
	}
}

// stop cancels the worker; Run drains and exits.
func (w *Worker) stop() { w.cancel() }

// requestStop asks the pipeline to wind up at the next safe boundary: the
// session loop ends its segment after the turn in flight, and drive() parks the
// run instead of advancing. Safe from any goroutine and on an idle worker - a
// request nothing consumes is cleared when the next drive starts.
func (w *Worker) requestStop() { w.stopReq.Store(true) }

// stopRequested reports whether a stop is pending.
func (w *Worker) stopRequested() bool { return w.stopReq.Load() }

// clearStopRequest consumes the request once it has been acted on. Stays pure:
// callers announce a stop themselves via publishStopLanded once the stop has
// actually settled. Two of the call sites are stale drops rather than landings,
// and the one that does park the run clears the flag before parking it, so
// publishing from here would announce a stop that has not happened yet.
func (w *Worker) clearStopRequest() { w.stopReq.Store(false) }

// sessionBusy tracks a session entering and leaving its run loop. Wired into
// every stage session by newStageSession.
func (w *Worker) sessionBusy(active bool) {
	if active {
		w.sessionsInFlight.Add(1)
		return
	}
	w.sessionsInFlight.Add(-1)
}

// working reports whether a stop would reach anything: a build in progress
// (including the validator sweep after the build session, where no session is open) or any session mid-flight.
func (w *Worker) working() bool {
	return w.State() == StateBuilding || w.sessionsInFlight.Load() > 0
}

// parkRun pauses the active run with defaultReason, but preserves a more
// specific reason a tool already wrote to of_runs (an approval or question) by
// reloading the run rather than clobbering it. mirrorMode also flips
// projects.mode to paused so the state is sticky and the dropdown agrees.
func (w *Worker) parkRun(defaultReason string, mirrorMode bool) {
	// Only a run that is still active can be paused. After go-live w.run still
	// points at the finished build run, and pausing that would reopen it; a LIVE
	// or scheduled question parks the worker with no run behind it.
	cur, err := activeRun(w.pdb)
	if w.run != nil && (err != nil || (cur != nil && cur.ID == w.run.ID)) {
		reason := defaultReason
		if cur != nil && cur.Status == RunStatusPaused && cur.PauseReason != "" {
			reason = cur.PauseReason
		}
		if err := w.run.pause(w.pdb, reason); err != nil {
			w.logger.Warn("pause run failed", "error", err)
		}
	}
	if mirrorMode {
		w.markModePaused()
	}
	w.setState(StatePaused)
}

// pauseOwnerStop parks on an owner stop (no question outstanding) and mirrors the
// pause into projects.mode so autoRecover resumes it on the next flip back to
// building. pauseAwaitingAnswers parks for owner input without mirroring.
func (w *Worker) pauseOwnerStop()       { w.parkRun(PauseReasonOwnerStop, true) }
func (w *Worker) pauseAwaitingAnswers() { w.parkRun(PauseReasonOwnerAnswers, false) }

// goLive flips the project mode to live and announces it.
func (w *Worker) goLive() {
	if err := w.setProjectMode(models.ModeLive); err != nil {
		w.logger.Warn("set project live failed", "error", err)
	}
	w.setState(StateLive)
}

// setProjectMode persists the owner-facing mode (projects.mode). The phase
// event that follows the next setState announces it. The caller logs a failure.
func (w *Worker) setProjectMode(mode string) error {
	_, err := w.deps.DB.ExecWrite(
		"UPDATE projects SET mode = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?", mode, w.projectID)
	return err
}

// stateForMode is the worker state a stored project mode starts in.
func stateForMode(mode string) State {
	switch mode {
	case models.ModeBuilding:
		return StateBuilding
	case models.ModeLive:
		return StateLive
	case models.ModePaused:
		return StatePaused
	}
	return StateIdle
}

// Run is the worker's main loop; call in its own goroutine.
func (w *Worker) Run() {
	ctx := w.ctx
	w.logger.Info("engine worker started")
	w.publishEvent(events.EventEngineStarted, map[string]any{
		"project_id": w.projectID,
	})
	defer func() {
		w.dbLog.Close()
		w.logger.Info("engine worker stopped")
		w.publishEvent(events.EventEngineStopped, map[string]any{
			"project_id": w.projectID,
		})
	}()

	project, err := models.GetProjectByID(w.deps.DB.DB, w.projectID)
	if err != nil {
		w.logger.Error("failed to load project", "error", err)
		return
	}

	w.setState(stateForMode(project.Mode))
	if w.State() == StateBuilding {
		time.Sleep(200 * time.Millisecond) // WAL checkpoint grace on fresh projects
		w.drive(ctx)
		w.answerLeftoverInbox(ctx)
	}
	w.commandLoop(ctx)
}

// commandLoop is the ambient state: drain commands; a command that flips the
// worker into building re-enters drive.
func (w *Worker) commandLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-w.commands:
			w.handleCommandSafely(ctx, cmd)
			if w.State() == StateBuilding {
				w.drive(ctx)
				w.answerLeftoverInbox(ctx)
			}
		}
	}
}

// handleCommandSafely runs one command with panic recovery. LIVE chat wakes, live
// resumes, and scheduled tasks run sessions from here rather than through the
// drive loop's runStageSafely, so without this a panic in one of them would unwind
// the worker goroutine and leave the project with no worker until the next nudge.
// Per-session bookkeeping (in-flight count, scheduled-task dedupe) is released by
// defers on the way out, so the worker is safe to keep serving.
func (w *Worker) handleCommandSafely(ctx context.Context, cmd Command) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("command panicked", "type", cmd.Type, "panic", r, "stack", string(debug.Stack()))
			w.publishError(fmt.Sprintf("Something went wrong while handling that (%v). I'm still here, so give it another try.", r))
		}
	}()
	w.handleCommand(ctx, cmd)
}

// markModePaused mirrors a terminal failure pause into projects.mode so the
// admin dropdown reflects reality. Owner-answer pauses skip this (they
// auto-resume on the next wake).
func (w *Worker) markModePaused() {
	if err := w.setProjectMode(models.ModePaused); err != nil {
		w.logger.Warn("failed to mirror pause into projects.mode", "error", err)
	}
}

// publishError sends an owner-visible error message + event.
func (w *Worker) publishError(errMsg string) {
	// The chat line and the event carry the same words, in the owner's language.
	errMsg = msgcat.Translate(w.replyLanguage(), errMsg)
	w.publishMessage("**Pipeline Error:** " + errMsg)
	w.publishEvent(events.EventEngineError, map[string]any{
		"error": errMsg,
	})
}

// answerLeftoverInbox answers owner messages a build ended without taking (it stopped
// on the turn they arrived, or they came during the validator sweep) with one LIVE wake.
func (w *Worker) answerLeftoverInbox(ctx context.Context) {
	msgs := w.takeOwnerInbox()
	if len(msgs) == 0 || w.State() == StateBuilding {
		if len(msgs) > 0 {
			for _, m := range msgs {
				w.pushOwnerInbox(m)
			}
		}
		return
	}
	w.handleCommandSafely(ctx, Command{Type: CommandChat, Payload: map[string]any{"message": strings.Join(msgs, "\n\n")}})
}

// pushOwnerInbox queues an owner message that arrived during a build.
func (w *Worker) pushOwnerInbox(msg string) {
	w.inboxMu.Lock()
	defer w.inboxMu.Unlock()
	w.ownerInbox = append(w.ownerInbox, msg)
}

// takeOwnerInbox empties the inbox, returning the queued messages oldest first.
func (w *Worker) takeOwnerInbox() []string {
	w.inboxMu.Lock()
	defer w.inboxMu.Unlock()
	msgs := w.ownerInbox
	w.ownerInbox = nil
	return msgs
}

// ownerInboxNote renders queued owner messages as the one user turn the build
// session is handed between turns. How to act on it is the build frame's rule.
//
// replyDirective is the project's current reply-language directive. The session's
// opening context was written when the build started, so it can be stale by now (the
// owner may have changed their language setting since); the note therefore carries
// the current one itself, once, after the messages.
func ownerInboxNote(msgs []string, replyDirective string) string {
	if len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ownerDuringBuildTag + " " + strings.TrimSpace(m))
	}
	if reminder := replylang.Reminder(replyDirective); reminder != "" {
		b.WriteByte('\n')
		b.WriteString(reminder)
	}
	return b.String()
}

// ownerDuringBuildTag marks an owner message delivered into a running build.
const ownerDuringBuildTag = "[owner message during build]"
