// Package jobs is a per-project background job runner. It polls the of_jobs
// table on each project database and executes generic jobs (send_email,
// http_request, run_sql, llm_prompt). It has no stages and no relation to the
// build engine; jobs are enqueued from action runners or LLM tools and
// dequeued here.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/actionexec"
	"openfabrica/internal/netguard"
	"openfabrica/internal/safego"
	"openfabrica/internal/sqlguard"
	"openfabrica/providers/llm"
	"openfabrica/security"
	"openfabrica/tools"
)

const jobPollInterval = 5 * time.Second

// maxJobsPerTick bounds how many due jobs one poll drains, so a backlog clears
// quickly without one project starving the process.
const maxJobsPerTick = 20

// Deps holds the shared dependencies job workers need.
type Deps struct {
	DB               *db.DB
	ProjectDBManager *db.ProjectDBManager
	Encryptor        *security.Encryptor
	// ToolExecutor and ToolEnv run the send_email job through manage_email with
	// the same deadline, panic recovery, and ToolContext shape as every other
	// tool caller.
	ToolExecutor *tools.Executor
	ToolEnv      *tools.Env
	// BudgetFn returns the per-project daily token budget (0 = unlimited). The
	// llm_prompt job gates on it against the shared unattended-spend tally so a
	// scheduled AI job cannot bill past the daily limit. Optional (nil = no gate).
	BudgetFn func() int
	// CallLLM runs llm_prompt completions under the engine's retry policy
	// (engine.Engine.CallLLM). Nil calls the provider directly.
	CallLLM llm.CallFunc
	// LLMHTTPClient is the instance's shared LLM client. Optional.
	LLMHTTPClient *http.Client
}

// Manager owns one Worker per active project, mirroring the engine's
// per-project worker lifecycle.
type Manager struct {
	mu      sync.Mutex
	deps    *Deps
	rootCtx context.Context
	workers map[int]context.CancelFunc
	wg      sync.WaitGroup
	logger  *slog.Logger
}

// NewManager creates a job manager. Call AutoStart to spawn workers for
// every active project, or StartProject to spawn one on demand.
func NewManager(deps *Deps, ctx context.Context) *Manager {
	return &Manager{
		deps:    deps,
		rootCtx: ctx,
		workers: make(map[int]context.CancelFunc),
		logger:  slog.With("component", "jobs_manager"),
	}
}

// StartProject spawns a job worker for the given project if one isn't already
// running. Idempotent.
func (m *Manager) StartProject(projectID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.workers[projectID]; exists {
		return nil
	}

	projectDB, err := m.deps.ProjectDBManager.Open(projectID)
	if err != nil {
		return fmt.Errorf("jobs: open project db %d: %w", projectID, err)
	}

	ctx, cancel := context.WithCancel(m.rootCtx)
	w := &Worker{
		projectID:     projectID,
		projectDB:     projectDB,
		globalDB:      m.deps.DB.DB,
		toolExec:      m.deps.ToolExecutor,
		toolEnv:       m.deps.ToolEnv.Headless(),
		encryptor:     m.deps.Encryptor,
		budgetFn:      m.deps.BudgetFn,
		callLLM:       m.deps.CallLLM,
		llmHTTPClient: m.deps.LLMHTTPClient,
		client:        netguard.SSRFSafeClient(),
		logger:        slog.With("component", "job_worker", "project_id", projectID),
	}
	m.workers[projectID] = cancel

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		// A panic in job polling/execution must not crash the process; recover
		// and let this project's job worker exit (the next StartProject revives it).
		safego.Run(fmt.Sprintf("job-worker[%d]", projectID), func() { w.Run(ctx) })
	}()

	m.logger.Info("job worker started", "project_id", projectID)
	return nil
}

// HasWorker reports whether a job worker is running for the project.
func (m *Manager) HasWorker(projectID int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.workers[projectID]
	return ok
}

// StopProject cancels the job worker for a single project, if one is running.
// Called before a project's database is torn down so the worker stops issuing
// queries against it. Idempotent.
func (m *Manager) StopProject(projectID int) {
	m.mu.Lock()
	cancel, ok := m.workers[projectID]
	if ok {
		delete(m.workers, projectID)
	}
	m.mu.Unlock()
	if ok {
		cancel()
		m.logger.Info("job worker stopped", "project_id", projectID)
	}
}

// StopAll cancels every worker and waits up to 30s for them to drain.
func (m *Manager) StopAll() {
	m.mu.Lock()
	for _, cancel := range m.workers {
		cancel()
	}
	m.workers = make(map[int]context.CancelFunc)
	m.mu.Unlock()

	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		m.logger.Warn("job workers did not stop within 30s")
	}
}

// AutoStart spawns workers for every runnable project.
func (m *Manager) AutoStart() error {
	projects, err := models.ListRunnableProjects(m.deps.DB.DB)
	if err != nil {
		return fmt.Errorf("jobs: list runnable projects: %w", err)
	}
	for _, s := range projects {
		if err := m.StartProject(s.ID); err != nil {
			m.logger.Error("failed to start job worker", "project_id", s.ID, "error", err)
		}
	}
	return nil
}

// Worker polls a single project's of_jobs table and runs due jobs.
type Worker struct {
	projectID     int
	projectDB     *db.ProjectDB
	globalDB      *sql.DB
	toolExec      *tools.Executor
	toolEnv       *tools.Env
	encryptor     *security.Encryptor
	budgetFn      func() int
	callLLM       llm.CallFunc
	llmHTTPClient *http.Client
	client        *http.Client // shared SSRF-hardened client for http_request jobs
	logger        *slog.Logger
}

func (w *Worker) Run(ctx context.Context) {
	w.recoverInterrupted()
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < maxJobsPerTick && ctx.Err() == nil; i++ {
				if !w.processPending(ctx) {
					break
				}
			}
		}
	}
}

// recoverInterrupted settles jobs a previous process left 'running'. This worker
// is the only one for its project, so nothing is legitimately running at start:
// a job with attempts left goes back to pending, the rest are failed.
func (w *Worker) recoverInterrupted() {
	if _, err := w.projectDB.ExecWrite(
		`UPDATE of_jobs SET status = CASE WHEN attempts < max_attempts THEN 'pending' ELSE 'failed' END,
		   error = 'interrupted by restart', completed_at = CASE WHEN attempts < max_attempts THEN NULL ELSE CURRENT_TIMESTAMP END
		 WHERE status = 'running'`,
	); err != nil {
		w.logger.Warn("could not recover interrupted jobs", "error", err)
	}
}

// processPending runs the next due job and reports whether one was taken.
func (w *Worker) processPending(ctx context.Context) bool {
	var jobID int64
	var jobType, payload string
	// datetime() normalizes ISO timestamps (T separator, Z or offset) to UTC, so a
	// job enqueued with an ISO scheduled_at is compared as a time, not as text. An
	// unparseable value falls back to due-now rather than never running.
	err := w.projectDB.Writer().QueryRow(
		`SELECT id, type, payload FROM of_jobs
		 WHERE status = 'pending' AND COALESCE(datetime(scheduled_at), '0000-01-01 00:00:00') <= datetime('now')
		 ORDER BY id ASC LIMIT 1`,
	).Scan(&jobID, &jobType, &payload)
	if err != nil {
		return false
	}

	result, err := w.projectDB.ExecWrite(
		"UPDATE of_jobs SET status = 'running', started_at = CURRENT_TIMESTAMP, attempts = attempts + 1 WHERE id = ? AND status = 'pending'",
		jobID,
	)
	if err != nil {
		return false
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return true
	}

	w.logger.Info("processing job", "job_id", jobID, "type", jobType)

	jobErr := w.executeJob(ctx, jobID, jobType, payload)
	if jobErr != nil {
		w.handleFailure(jobID, jobErr)
	} else {
		// Guarded on 'running' so an admin cancel during execution is not overwritten.
		if _, err := w.projectDB.ExecWrite("UPDATE of_jobs SET status = 'completed', completed_at = CURRENT_TIMESTAMP WHERE id = ? AND status = 'running'", jobID); err != nil {
			w.logger.Error("failed to mark job completed", "job_id", jobID, "error", err)
		}
		w.logger.Info("job completed", "job_id", jobID)
	}
	return true
}

func (w *Worker) executeJob(ctx context.Context, jobID int64, jobType, payload string) error {
	var params map[string]any
	if err := json.Unmarshal([]byte(payload), &params); err != nil {
		return fmt.Errorf("invalid payload JSON: %w", err)
	}
	switch jobType {
	case "send_email":
		return w.executeSendEmail(ctx, params)
	case "http_request":
		return w.executeHTTPRequest(ctx, params)
	case "run_sql":
		return w.executeRunSQL(ctx, params)
	case "llm_prompt":
		return w.executeLLMPrompt(ctx, jobID, params)
	default:
		return fmt.Errorf("unknown job type: %s", jobType)
	}
}

// executeLLMPrompt runs a one-shot completion on the project's model and stores the
// response text in the job's result column. The completion path is
// actionexec.Complete, the same one a trigger's llm_call takes.
func (w *Worker) executeLLMPrompt(ctx context.Context, jobID int64, params map[string]any) error {
	prompt, _ := params["prompt"].(string)
	if prompt == "" {
		return fmt.Errorf("llm_prompt: 'prompt' is required")
	}
	system, _ := params["system"].(string)
	maxTokens := 512
	if v, ok := params["max_tokens"].(float64); ok && v > 0 {
		maxTokens = int(v)
	}

	content, err := actionexec.Complete(ctx, actionexec.CompleteDeps{
		GlobalDB:   w.globalDB,
		Encryptor:  w.encryptor,
		HTTPClient: w.llmHTTPClient,
		BudgetFn:   w.budgetFn,
		Call:       w.callLLM,
	}, w.projectID, w.projectDB.Writer(), "job", actionexec.CompleteRequest{
		Prompt: prompt, System: system, MaxTokens: maxTokens, Timeout: 90 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("llm_prompt: %w", err)
	}

	if _, err := w.projectDB.ExecWrite("UPDATE of_jobs SET result = ? WHERE id = ?", content, jobID); err != nil {
		w.logger.Warn("llm_prompt: failed to store result", "job_id", jobID, "error", err)
	}
	return nil
}

// executeSendEmail dispatches through the manage_email tool. The send-arg
// shaping is shared with the trigger runner via internal/actionexec; the
// worker dispatches through the shared tools.Executor.
func (w *Worker) executeSendEmail(ctx context.Context, params map[string]any) error {
	return actionexec.SendEmail(params, func(toolName string, args map[string]any) error {
		if w.toolExec == nil || w.toolEnv == nil {
			return fmt.Errorf("%s: tool executor not wired into the job worker", toolName)
		}
		result, _, err := w.toolExec.Invoke(ctx, w.toolEnv, tools.Call{
			Name:      toolName,
			Args:      args,
			ProjectID: w.projectID,
			Project:   w.projectDB,
			Logger:    w.logger,
		})
		if err != nil {
			return err
		}
		if !result.Success {
			return fmt.Errorf("email send failed: %s", result.Error)
		}
		return nil
	})
}

// executeHTTPRequest sends an HTTP request via the executor shared with the
// trigger runner. SSRF guard: a job's URL can come from a templated
// enqueue_job action, so it is untrusted. netguard.ValidateExternalURL rejects
// private/internal targets pre-flight and the shared hardened client
// re-validates on redirect and checks the dial-time IP.
func (w *Worker) executeHTTPRequest(ctx context.Context, params map[string]any) error {
	return actionexec.HTTPRequest(ctx, w.client, params, actionexec.HTTPOptions{
		DefaultMethod: http.MethodGet, // historical jobs default; triggers default to POST
		ValidateURL:   netguard.ValidateExternalURL,
	})
}

func (w *Worker) executeRunSQL(ctx context.Context, params map[string]any) error {
	// A job's SQL can originate from a templated enqueue_job action, so it is read-only
	// and guarded the way every stored SQL action is (sqlguard.CheckAction).
	query, _ := params["sql"].(string)
	if _, err := sqlguard.CheckAction(query, "SELECT", "VALUES"); err != nil {
		return fmt.Errorf("run_sql: %w", err)
	}
	// run_sql is a result-discarding smoke/side-effect query: nothing consumes
	// the rows, but the job must still drain them so a mid-iteration error fails
	// the job instead of vanishing.
	rows, err := w.projectDB.Query(query)
	if err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()
	rowCount := 0
	for rows.Next() {
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query failed: %w", err)
	}
	w.logger.Debug("run_sql job query drained", "rows", rowCount)
	return nil
}

func (w *Worker) handleFailure(jobID int64, jobErr error) {
	var attempts, maxAttempts int
	if err := w.projectDB.Writer().QueryRow("SELECT attempts, max_attempts FROM of_jobs WHERE id = ?", jobID).Scan(&attempts, &maxAttempts); err != nil {
		// Can't read the retry counters, so we can't decide retry vs. permanent
		// failure. Leave the row untouched and log; it is settled on the next restart.
		w.logger.Error("job failure handler could not read retry counters", "job_id", jobID, "error", err, "job_error", jobErr)
		return
	}

	if attempts < maxAttempts {
		backoffSecs := int(math.Pow(2, float64(attempts-1)) * 5)
		if backoffSecs > 300 {
			backoffSecs = 300
		}
		if _, err := w.projectDB.ExecWrite(
			"UPDATE of_jobs SET status = 'pending', error = ?, scheduled_at = datetime('now', ? || ' seconds') WHERE id = ? AND status = 'running'",
			jobErr.Error(), fmt.Sprintf("+%d", backoffSecs), jobID,
		); err != nil {
			w.logger.Error("failed to reschedule job retry", "job_id", jobID, "error", err)
		}
		w.logger.Warn("job failed, will retry", "job_id", jobID, "attempt", attempts, "backoff_s", backoffSecs, "error", jobErr)
		return
	}
	if _, err := w.projectDB.ExecWrite("UPDATE of_jobs SET status = 'failed', error = ?, completed_at = CURRENT_TIMESTAMP WHERE id = ? AND status = 'running'", jobErr.Error(), jobID); err != nil {
		w.logger.Error("failed to mark job failed", "job_id", jobID, "error", err)
	}
	w.logger.Error("job permanently failed", "job_id", jobID, "attempts", attempts, "error", jobErr)
}
