package engine

// Engine: per-project worker lifecycle. Owns the worker map, the shared
// per-(provider,model) LLM concurrency limiter, and background maintenance
// (replay-cache + call-log pruning).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"openfabrica/db/models"
	"openfabrica/internal/flowhooks"
	"openfabrica/internal/safego"
	"openfabrica/internal/sqliteretry"
)

// workerHandle pairs a worker with its done channel (closed when Run returns).
type workerHandle struct {
	worker *Worker
	done   chan struct{}
}

// Engine manages workers for all active projects.
type Engine struct {
	mu              sync.RWMutex
	workers         map[int]*workerHandle
	wg              sync.WaitGroup
	deps            *Deps
	limiter         *llmLimiter
	logger          *slog.Logger
	rootCtx         context.Context
	closed          bool      // set by StopAll; refuses new workers so wg is not reused mid-Wait
	schedulerOnce   sync.Once // guards StartScheduler against a duplicate supervised loop
	maintenanceOnce sync.Once // guards StartMaintenance against a duplicate supervised loop
}

// New creates the engine.
func New(deps *Deps, ctx context.Context) *Engine {
	e := &Engine{
		workers: make(map[int]*workerHandle),
		deps:    deps,
		limiter: newLLMLimiter(deps.MaxConcurrentPerModel),
		logger:  slog.With("component", "engine"),
		rootCtx: ctx,
	}
	// User-flow replays run a project's scheduled tasks through the engine that
	// owns its workers (runSchedule steps).
	flowhooks.SetScheduleRunner(e.runScheduleForFlow)
	return e
}

// StartProject starts a worker for the project (no-op when already running).
func (e *Engine) StartProject(projectID int) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Refuse to start during shutdown: a late StartProject (reachable via the
	// project-created/bot paths) could call wg.Add while StopAll's wg.Wait is
	// draining to zero, which panics on a reused WaitGroup.
	if e.closed {
		return fmt.Errorf("engine: shutting down, refusing to start project %d", projectID)
	}

	if _, exists := e.workers[projectID]; exists {
		e.logger.Debug("worker already running", "project_id", projectID)
		return nil
	}
	worker, err := newWorker(e.deps, projectID, e.limiter, e.rootCtx)
	if err != nil {
		return fmt.Errorf("engine: failed to create worker for project %d: %w", projectID, err)
	}
	done := make(chan struct{})
	e.workers[projectID] = &workerHandle{worker: worker, done: done}

	handle := e.workers[projectID]
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(done)
		// Deregister BEFORE closing done (defers run LIFO), so a StopProject
		// waiting on done never observes a stopped worker still in the map.
		defer e.forget(projectID, handle)
		// A panic in the worker's drive scaffolding must not crash the whole
		// process and take every other project down with it; recover, log, and
		// let this one worker exit (its project lands idle until nudged again).
		safego.Run(fmt.Sprintf("engine-worker[%d]", projectID), worker.Run)
	}()
	e.logger.Info("worker started", "project_id", projectID)
	return nil
}

// forget removes a worker from the map once its goroutine has returned, but only
// when the map still holds THIS handle: a project stopped and restarted has a new
// handle, and the old goroutine's teardown must not evict its successor.
func (e *Engine) forget(projectID int, handle *workerHandle) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.workers[projectID] == handle {
		delete(e.workers, projectID)
	}
}

// stopProjectTimeout bounds StopProject's wait for a draining worker. A var so
// tests can shorten it; nothing in production writes it.
var stopProjectTimeout = 30 * time.Second

// ErrNoWorker reports that a project had no running worker. Distinct from a
// stop that FAILED, because the two mean opposite things to project-delete: no
// worker is the ordinary case for an idle project, while a timeout means a live
// worker is about to have its database deleted underneath it.
var ErrNoWorker = errors.New("engine: no worker running for project")

// StopProject stops the worker and blocks until it returns (bounded).
// Blocking is load-bearing: project-delete tears down the project DB right
// after this returns.
//
// The map entry is NOT removed here. The worker deregisters itself when its
// goroutine returns (see forget), so a worker that misses the deadline stays
// reachable. Evicting it on timeout would leave a live goroutine no later
// StopProject or StopAll could find, still driving a project whose row and
// database are about to be deleted.
func (e *Engine) StopProject(projectID int) error {
	e.mu.Lock()
	handle, exists := e.workers[projectID]
	e.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w %d", ErrNoWorker, projectID)
	}
	handle.worker.stop()

	select {
	case <-handle.done:
		e.logger.Info("worker stopped", "project_id", projectID)
		return nil
	case <-time.After(stopProjectTimeout):
		e.logger.Warn("worker did not stop within timeout; it stays registered and will deregister itself when it returns",
			"project_id", projectID)
		return fmt.Errorf("engine: worker for project %d did not stop within %s", projectID, stopProjectTimeout)
	}
}

// StopAll stops every worker and waits (bounded).
func (e *Engine) StopAll() {
	e.mu.Lock()
	e.closed = true
	for projectID, handle := range e.workers {
		handle.worker.stop()
		e.logger.Info("worker stopping", "project_id", projectID)
	}
	// The map is left alone: each worker clears its own entry as it returns, and
	// wg.Wait below is what actually bounds the drain.
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		e.logger.Info("all workers stopped")
	case <-time.After(30 * time.Second):
		e.logger.Warn("workers did not stop within 30s")
	}
}

// RequestStop asks a project's pipeline to stop at the next safe boundary,
// leaving the worker alive and the run resumable. Bypasses the command channel
// on purpose: that channel is drained only between drives, so a stop sent
// through it would arrive after the work it was meant to interrupt. Rejects a
// stop nothing would consume: an idle worker's flag would sit unread, leaving
// the caller's UI holding a stop that never lands. Use StopProject to tear the
// worker down entirely.
func (e *Engine) RequestStop(projectID int) error {
	e.mu.RLock()
	handle, exists := e.workers[projectID]
	e.mu.RUnlock()
	if !exists {
		return fmt.Errorf("engine: no worker running for project %d", projectID)
	}
	if !handle.worker.working() {
		return fmt.Errorf("engine: nothing running to stop for project %d", projectID)
	}
	handle.worker.requestStop()
	e.logger.Info("stop requested", "project_id", projectID)
	return nil
}

// Status returns the worker's state, or idle when none runs.
func (e *Engine) Status(projectID int) State {
	e.mu.RLock()
	defer e.mu.RUnlock()
	handle, exists := e.workers[projectID]
	if !exists {
		return StateIdle
	}
	return handle.worker.State()
}

// Send delivers a command to a project's worker.
func (e *Engine) Send(projectID int, cmd Command) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	handle, exists := e.workers[projectID]
	if !exists {
		return fmt.Errorf("engine: no worker running for project %d", projectID)
	}
	return handle.worker.Send(cmd)
}

// IsRunning reports whether a worker exists for the project.
func (e *Engine) IsRunning(projectID int) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, exists := e.workers[projectID]
	return exists
}

// RunningCount returns the number of running workers.
func (e *Engine) RunningCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.workers)
}

// RunningProjects returns the ids of projects with active workers.
func (e *Engine) RunningProjects() []int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]int, 0, len(e.workers))
	for id := range e.workers {
		ids = append(ids, id)
	}
	return ids
}

// AutoStart starts a worker for every runnable project.
func (e *Engine) AutoStart() error {
	projects, err := models.ListRunnableProjects(e.deps.DB.DB)
	if err != nil {
		return fmt.Errorf("engine: failed to list runnable projects: %w", err)
	}
	e.logger.Info("auto-starting workers", "projects", len(projects))
	for _, project := range projects {
		if err := e.StartProject(project.ID); err != nil {
			e.logger.Error("failed to start worker", "project_id", project.ID, "error", err)
		}
	}
	return nil
}

// StartScheduler starts the scheduled-task dispatcher: next_run bookkeeping,
// of_task_runs recording, and CommandScheduledTask dispatch with in-flight
// dedup (see scheduled.go).
func (e *Engine) StartScheduler() {
	// Supervised: a panic while dispatching a malformed scheduled task must not
	// silently kill the dispatcher (or the process). Restart until shutdown.
	// Once-guarded so a second call cannot spawn a duplicate dispatcher.
	e.schedulerOnce.Do(func() {
		safego.Supervise(e.rootCtx, "engine-scheduler", e.schedulerLoop)
		e.logger.Info("task scheduler started")
	})
}

// maintenance tuning.
const (
	llmCallRetention     = 30 * 24 * time.Hour
	replayCacheRetention = 30 * 24 * time.Hour
)

// StartMaintenance runs daily pruning of of_llm_calls and of_llm_cache across
// active projects.
func (e *Engine) StartMaintenance() {
	e.maintenanceOnce.Do(e.startMaintenance)
}

func (e *Engine) startMaintenance() {
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		select {
		case <-e.rootCtx.Done():
			return
		case <-time.After(5 * time.Minute):
			safego.Run("engine-maintenance", e.runMaintenance)
		}
		for {
			select {
			case <-e.rootCtx.Done():
				return
			case <-ticker.C:
				safego.Run("engine-maintenance", e.runMaintenance)
			}
		}
	}()
	e.logger.Info("maintenance goroutine started")
}

func (e *Engine) runMaintenance() {
	projects, err := models.ListProjects(e.deps.DB.DB)
	if err != nil {
		e.logger.Error("maintenance: failed to list projects", "error", err)
		return
	}
	for _, project := range projects {
		pdb, err := e.deps.ProjectDBManager.Open(project.ID)
		if err != nil {
			continue
		}
		if n, err := pruneLLMCalls(pdb, llmCallRetention); err == nil && n > 0 {
			e.logger.Info("pruned old llm calls", "project_id", project.ID, "deleted", n)
		}
		if n, err := pruneReplayCache(pdb, replayCacheRetention); err == nil && n > 0 {
			e.logger.Info("pruned replay cache", "project_id", project.ID, "deleted", n)
		}
		if gone, err := pruneRetained(pdb, time.Now()); err != nil {
			e.logger.Warn("retention prune failed", "project_id", project.ID, "error", err)
		} else if len(gone) > 0 {
			e.logger.Info("pruned old rows", "project_id", project.ID, "deleted", gone)
		}
		// Reconcile orphaned task runs: a worker that stopped between dispatch
		// and finalizeTaskRun leaves an of_task_runs row stuck in 'running'
		// forever. Fail any that have been running far longer than a single
		// action could legitimately take.
		if res, err := sqliteretry.Exec(pdb.Writer(),
			`UPDATE of_task_runs SET status = 'failed', error_message = 'orphaned: worker stopped before the run finished', completed_at = CURRENT_TIMESTAMP
			 WHERE status = 'running' AND started_at < datetime('now', '-6 hours')`); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				e.logger.Info("failed orphaned task runs", "project_id", project.ID, "count", n)
			}
		}
	}
}
