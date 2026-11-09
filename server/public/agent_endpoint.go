package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/agentrt"
	"openfabrica/internal/funcexec"
	"openfabrica/internal/funcsandbox"
	"openfabrica/internal/httpx"
	"openfabrica/internal/safego"
	"openfabrica/providers/llm"
	"openfabrica/server/middleware"
)

// Agent endpoint handler (manage_agent).
//
// The multi-step sibling of the LLM proxy: where apiLLM returns one completion,
// apiAgent runs a bounded tool-calling loop (internal/agentrt) on the project's
// LLM. The agent's tools are the app's OWN capabilities, allowlisted on the
// endpoint row: each manage_function endpoint named in `functions`, plus an
// opt-in read-only data-query tool. It reuses the proxy's auth, rate-limit,
// daily-budget, provider-resolution, and logging machinery (llm_endpoint.go).
//
// Phase 1 is synchronous: the whole run completes inside the request. A run that
// outlasts the request deadline fails with the provider's context error; an
// async job-backed mode is the planned follow-up.

// agentMaxHistory caps the conversation the caller may seed, so a huge history
// can't inflate every step's input. The daily budget still bounds total spend.
const agentMaxHistory = 40

// agentEndpointRow is the stored config for one agent endpoint.
type agentEndpointRow struct {
	Path             string
	SystemPrompt     string
	ModelID          string
	MaxTokens        int
	Temperature      float64
	MaxSteps         int
	RateLimit        int
	DailyTokenBudget int
	DailyTokensUsed  int
	DayWindowStart   string
	RequiresAuth     bool
	CORSOrigins      string
	Functions        string // JSON array of manage_function endpoint paths
	AllowDataQuery   bool
	AllowHTTPFetch   bool
	MaxSubagents     int
	MaxDepth         int
}

func (h *Handler) loadAgentEndpoint(reader *sql.DB, path string) (*agentEndpointRow, error) {
	var ep agentEndpointRow
	err := reader.QueryRow(
		"SELECT path, system_prompt, model_id, max_tokens, temperature, max_steps, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, requires_auth, cors_origins, functions, allow_data_query, max_subagents, max_depth, allow_http_fetch FROM of_agent_endpoints WHERE path = ?",
		path,
	).Scan(&ep.Path, &ep.SystemPrompt, &ep.ModelID, &ep.MaxTokens, &ep.Temperature, &ep.MaxSteps, &ep.RateLimit, &ep.DailyTokenBudget, &ep.DailyTokensUsed, &ep.DayWindowStart, &ep.RequiresAuth, &ep.CORSOrigins, &ep.Functions, &ep.AllowDataQuery, &ep.MaxSubagents, &ep.MaxDepth, &ep.AllowHTTPFetch)
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// checkAndResetAgentDailyBudget rolls the per-endpoint UTC-day counter forward on
// the first hit of a new day, then reports whether the budget still has headroom.
// budget==0 means unlimited. Mirrors checkAndResetDailyBudget on the LLM proxy,
// minus the usage-archival table (agent analytics is a later addition).
func checkAndResetAgentDailyBudget(projectDB *sql.DB, ep *agentEndpointRow) (bool, int, error) {
	today := time.Now().UTC().Format("2006-01-02")
	if ep.DayWindowStart != today {
		if _, err := projectDB.Exec(
			"UPDATE of_agent_endpoints SET daily_tokens_used = 0, day_window_start = ? WHERE path = ?",
			today, ep.Path,
		); err != nil {
			return false, 0, err
		}
		ep.DayWindowStart = today
		ep.DailyTokensUsed = 0
	}
	if ep.DailyTokenBudget <= 0 {
		return true, ep.DailyTokensUsed, nil
	}
	return ep.DailyTokensUsed < ep.DailyTokenBudget, ep.DailyTokensUsed, nil
}

// recordAgentUsage adds a finished run's tokens to the per-endpoint daily
// counter. Best-effort: a failure is logged, not surfaced to the caller who
// already got their answer.
func recordAgentUsage(projectDB *sql.DB, path string, tokens int) {
	if tokens <= 0 {
		return
	}
	if _, err := projectDB.Exec(
		"UPDATE of_agent_endpoints SET daily_tokens_used = daily_tokens_used + ? WHERE path = ?",
		tokens, path,
	); err != nil {
		slog.Warn("agent endpoint: failed to record usage", "path", path, "tokens", tokens, "error", err)
	}
}

// apiAgent handles POST /api/{path}/run.
func (h *Handler) apiAgent(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *db.ProjectDB, endpointPath string) {
	reader, writer := projectDB.Reader(), projectDB.Writer()

	ep, err := h.loadAgentEndpoint(reader, endpointPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "agent endpoint not found")
		return
	}

	// Per-endpoint CORS on the real response; the OPTIONS preflight is answered
	// centrally upstream by handleAPIPreflight.
	if ep.CORSOrigins != "" {
		applyEndpointCORS(w, ep.CORSOrigins, r.Header.Get("Origin"), "POST, OPTIONS", "Content-Type, Authorization")
	}

	// Auth gate plus best-effort identity: an agent that calls the app's
	// functions passes the caller as req.user, exactly as a direct function call
	// would (function_endpoint.go).
	var user map[string]any
	// fullDataAccess is true for a project API key (owner-level access) and the admin
	// role, the two callers the REST path also exempts from owner/tenant scoping. The
	// agent's query_data tool withholds scoped tables from everyone else.
	fullDataAccess := false
	if ep.RequiresAuth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		ok, claims := h.validateProjectTokenOrJWTWithClaims(project.ID, token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if claims == nil {
			fullDataAccess = true
		} else if claims.UserID > 0 {
			user = map[string]any{"id": claims.UserID, "role": claims.Role}
			fullDataAccess = claims.Role == "admin"
		}
	}

	// Rate limit by IP.
	rateLimitKey := fmt.Sprintf("agent:%d:%s:%s", project.ID, ep.Path, middleware.RateLimitIdentity(r))
	if !checkEndpointRateLimit(rateLimitKey, ep.RateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	// Daily-token budget gate. A zero budget is unlimited, so the check no-ops
	// for endpoints that haven't opted in.
	allowed, used, err := checkAndResetAgentDailyBudget(writer, ep)
	if err != nil {
		slog.Error("agent endpoint: budget check failed", "project_id", project.ID, "path", ep.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "budget check failed")
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", "86400")
		writeError(w, http.StatusTooManyRequests,
			fmt.Sprintf("daily token budget exhausted (%d/%d used). Resets at 00:00 UTC.", used, ep.DailyTokenBudget))
		return
	}
	if !publicSpendAllowed(w, reader, project.ID, ep.Path) {
		return
	}

	// Parse and validate the conversation (same shape as the LLM proxy).
	var req llmChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxLLMBodySize)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var messages []llm.Message
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		messages = append(messages, llm.Message{Role: llm.Role(m.Role), Content: m.Content})
	}
	if len(messages) > agentMaxHistory {
		messages = messages[len(messages)-agentMaxHistory:]
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != llm.RoleUser {
		writeError(w, http.StatusBadRequest, "last message must be from user")
		return
	}

	// Resolve provider and log the run's calls under source="agent".
	provider, modelID, err := h.resolveLLMProvider(project.ID)
	if err != nil {
		slog.Error("agent endpoint: failed to resolve provider", "project_id", project.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "AI service not available")
		return
	}
	if ep.ModelID != "" {
		modelID = ep.ModelID
	}
	// The run budget is what's left of today's allowance, so a single run can
	// never spend past the daily cap. A zero daily budget leaves it unlimited
	// (nil), bounded then only by max_steps.
	var budget *agentrt.Budget
	if ep.DailyTokenBudget > 0 {
		budget = agentrt.NewBudget(ep.DailyTokenBudget - used)
	}

	temp := ep.Temperature
	cfg := agentrt.Config{
		Model:        modelID,
		System:       ep.SystemPrompt,
		MaxTokens:    ep.MaxTokens,
		Temperature:  &temp,
		MaxSteps:     ep.MaxSteps,
		Tools:        h.buildAgentTools(project.ID, projectDB, ep, user, fullDataAccess),
		Budget:       budget,
		MaxSubagents: ep.MaxSubagents,
		MaxDepth:     ep.MaxDepth,
	}

	// Async form (?async=1): a detached run that outlives this request, so a slow
	// agent survives a client disconnect; the caller polls GET /run?id=<id>. It
	// owns its own call logger, set up inside the goroutine rather than here where
	// the request scope (and the deferred Close) ends first.
	if r.URL.Query().Get("async") == "1" {
		h.startAgentRunAsync(w, project, projectDB, ep, cfg, provider, messages, modelID)
		return
	}

	// Sync and streaming share a request-scoped call logger.
	llmLogger := llm.NewDBLLMLogger(writer)
	defer llmLogger.Close()
	cfg.Provider = llm.NewLoggedProvider(provider, llmLogger, "agent", ep.Path, nil, nil)

	// Streaming form (?stream=1 or an SSE Accept header): emit a frame per step
	// then a terminal done frame. Lets a page show progress on a run too long
	// for one JSON round-trip.
	if wantsSSE(r) {
		h.runAgentSSE(w, r, writer, ep, cfg, messages, modelID)
		return
	}

	res, err := agentrt.Run(r.Context(), cfg, messages)
	if err != nil {
		slog.Error("agent endpoint: run failed", "project_id", project.ID, "path", ep.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "agent run failed")
		return
	}
	recordAgentUsage(writer, ep.Path, res.TotalTokens())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentRunResponse(res, modelID))
}

// agentRunResponse is the JSON body of a finished run, shared by the sync
// response and the SSE terminal `done` frame.
func agentRunResponse(res agentrt.Result, modelID string) map[string]any {
	return map[string]any{
		"content":     res.Text,
		"steps":       res.Steps,
		"stop_reason": res.StopReason,
		"subagents":   res.Subagents,
		"model":       modelID,
		"usage": map[string]int{
			"input_tokens":  res.InputTokens,
			"output_tokens": res.OutputTokens,
		},
	}
}

// wantsSSE reports whether the caller asked for the streaming form of /run.
func wantsSSE(r *http.Request) bool {
	if r.URL.Query().Get("stream") == "1" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// runAgentSSE streams a run as Server-Sent Events: one frame per step
// (tool_call / tool_result / text, each tagged with its sub-agent depth), then a
// terminal `done` frame carrying the final answer, stop_reason, sub-agent count,
// and usage. A hard run failure arrives as an `error` frame.
func (h *Handler) runAgentSSE(w http.ResponseWriter, r *http.Request, writer *sql.DB, ep *agentEndpointRow, cfg agentrt.Config, messages []llm.Message, modelID string) {
	sse, ok := httpx.NewSSE(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	cfg.OnEvent = func(e agentrt.Event) {
		switch e.Kind {
		case agentrt.EventToolCall:
			sseEvent(sse, "tool_call", map[string]any{"tool": e.Tool, "args": e.Text, "depth": e.Depth})
		case agentrt.EventToolResult:
			sseEvent(sse, "tool_result", map[string]any{"tool": e.Tool, "result": e.Text, "depth": e.Depth})
		case agentrt.EventText:
			sseEvent(sse, "text", map[string]any{"text": e.Text, "depth": e.Depth})
		case agentrt.EventError:
			// A non-fatal in-run notice (step cap, budget). Distinct from the
			// terminal `error` frame so the client does not treat it as a failure.
			sseEvent(sse, "notice", map[string]any{"message": e.Text, "depth": e.Depth})
		}
		// EventDone is superseded by the terminal done frame below.
	}
	res, err := agentrt.Run(r.Context(), cfg, messages)
	recordAgentUsage(writer, ep.Path, res.TotalTokens())
	if err != nil {
		sseEvent(sse, "error", map[string]string{"error": "agent run failed"})
		return
	}
	sseEvent(sse, "done", agentRunResponse(res, modelID))
}

// agentAsyncMaxWall bounds a detached run's wall-clock, a backstop beyond
// max_steps and the token budget. agentRunExpiry is how long a row may sit
// "running" before a poll reports it failed (its goroutine is gone, typically a
// server restart mid-run); set a hair past the wall-clock cap.
const (
	agentAsyncMaxWall = 15 * time.Minute
	agentRunExpiry    = 16 * time.Minute
)

// startAgentRunAsync records a run row, kicks off a detached goroutine to run the
// agent, and answers 202 with the id to poll. The goroutine owns its own call
// logger and a background context so it survives the request returning.
func (h *Handler) startAgentRunAsync(w http.ResponseWriter, project *models.Project, projectDB *db.ProjectDB, ep *agentEndpointRow, cfg agentrt.Config, provider llm.Provider, messages []llm.Message, modelID string) {
	runID := uuid.NewString()
	writer := projectDB.Writer()
	path := ep.Path

	reqJSON, _ := json.Marshal(messages)
	if _, err := writer.Exec(
		`INSERT INTO of_agent_runs (id, path, status, request) VALUES (?, ?, 'running', ?)`,
		runID, path, string(reqJSON),
	); err != nil {
		slog.Error("agent endpoint: failed to record async run", "project_id", project.ID, "path", path, "error", err)
		writeError(w, http.StatusInternalServerError, "could not start run")
		return
	}

	safego.Go(fmt.Sprintf("agent-run[%d:%s]", project.ID, runID), func() {
		ctx, cancel := context.WithTimeout(context.Background(), agentAsyncMaxWall)
		defer cancel()

		logger := llm.NewDBLLMLogger(writer)
		defer logger.Close()
		runCfg := cfg
		runCfg.Provider = llm.NewLoggedProvider(provider, logger, "agent", path, nil, nil)

		res, err := agentrt.Run(ctx, runCfg, messages)
		recordAgentUsage(writer, path, res.TotalTokens())
		if err != nil {
			if _, uerr := writer.Exec(
				`UPDATE of_agent_runs SET status='failed', error=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
				"agent run failed", runID,
			); uerr != nil {
				slog.Error("agent endpoint: failed to mark run failed", "run_id", runID, "error", uerr)
			}
			return
		}
		if _, uerr := writer.Exec(
			`UPDATE of_agent_runs SET status='done', content=?, stop_reason=?, steps=?, subagents=?, input_tokens=?, output_tokens=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
			res.Text, res.StopReason, res.Steps, res.Subagents, res.InputTokens, res.OutputTokens, runID,
		); uerr != nil {
			slog.Error("agent endpoint: failed to record run result", "run_id", runID, "error", uerr)
		}
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"run_id":     runID,
		"status":     "running",
		"status_url": fmt.Sprintf("/api/%s/run?id=%s", path, runID),
	})
}

// apiAgentRunStatus handles GET /api/{path}/run?id=<id>: the poll for an async
// run. Same auth gate as starting one.
func (h *Handler) apiAgentRunStatus(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *db.ProjectDB, endpointPath string) {
	reader := projectDB.Reader()
	ep, err := h.loadAgentEndpoint(reader, endpointPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "agent endpoint not found")
		return
	}
	if ep.CORSOrigins != "" {
		applyEndpointCORS(w, ep.CORSOrigins, r.Header.Get("Origin"), "GET, POST, OPTIONS", "Content-Type, Authorization")
	}
	runID := r.URL.Query().Get("id")
	if runID == "" {
		// A GET to /run with no ?id is not a poll. It is almost always a streaming
		// client built on EventSource (a GET with messages in the query string),
		// which cannot POST the body or send the Authorization header - so it lands
		// here and, on an auth-gated endpoint, would otherwise 401 with no clue why.
		// Answer with the fix BEFORE the auth gate, so the developer gets guidance.
		writeError(w, http.StatusBadRequest,
			"GET /api/"+endpointPath+"/run polls an async run and requires ?id=<id>. To start or STREAM a run, POST to /api/"+endpointPath+"/run and read the SSE response with fetch() + a ReadableStream reader - EventSource can neither POST the messages nor send the auth token.")
		return
	}

	if ep.RequiresAuth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !h.validateProjectTokenOrJWT(project.ID, token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	var (
		status, content, stopReason, runErr string
		steps, subagents, inTok, outTok     int
		ageSecs                             int
	)
	err = reader.QueryRow(
		`SELECT status, content, stop_reason, steps, subagents, input_tokens, output_tokens, error,
		        CAST(strftime('%s','now') - strftime('%s', updated_at) AS INTEGER)
		   FROM of_agent_runs WHERE id = ? AND path = ?`,
		runID, endpointPath,
	).Scan(&status, &content, &stopReason, &steps, &subagents, &inTok, &outTok, &runErr, &ageSecs)
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}

	// Read-time expiry: a row still 'running' well past the wall-clock cap means
	// its goroutine is gone (typically a server restart). Report it failed rather
	// than leave a poller waiting forever.
	if status == "running" && ageSecs > int(agentRunExpiry.Seconds()) {
		status = "failed"
		runErr = "run did not complete (the server may have restarted)"
	}

	body := map[string]any{"run_id": runID, "status": status}
	switch status {
	case "done":
		body["content"] = content
		body["stop_reason"] = stopReason
		body["steps"] = steps
		body["subagents"] = subagents
		body["usage"] = map[string]int{"input_tokens": inTok, "output_tokens": outTok}
	case "failed":
		body["error"] = runErr
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// buildAgentTools turns the endpoint's allowlist into the concrete tool set the
// loop dispatches: one tool per manage_function endpoint named in `functions`,
// plus the read-only data-query tool when allow_data_query is set.
func (h *Handler) buildAgentTools(projectID int, projectDB *db.ProjectDB, ep *agentEndpointRow, user map[string]any, fullDataAccess bool) []agentrt.Tool {
	var out []agentrt.Tool
	for _, fnPath := range parseFunctionAllowlist(ep.Functions) {
		out = append(out, h.agentFunctionTool(projectID, projectDB, fnPath, user))
	}
	if ep.AllowDataQuery {
		out = append(out, agentDataQueryTool(projectDB, fullDataAccess))
	}
	if ep.AllowHTTPFetch {
		out = append(out, agentHTTPFetchTool())
	}
	return out
}

// agentHTTPFetchTool exposes one SSRF-guarded outbound request, through the same
// validator and pinned-resolution client that back a function handler's
// ctx.fetch (funcexec.Fetch).
func agentHTTPFetchTool() agentrt.Tool {
	return agentrt.Tool{
		Def: llm.ToolDef{
			Name:        "http_fetch",
			Description: "Fetch an external URL and return its status, headers, and body (JSON). Outbound only - internal/loopback addresses are refused. Use for public APIs and pages.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":     map[string]any{"type": "string", "description": "Absolute http(s) URL to fetch."},
					"method":  map[string]any{"type": "string", "description": "HTTP method (default GET)."},
					"headers": map[string]any{"type": "object", "description": "Optional request headers."},
					"body":    map[string]any{"type": "string", "description": "Optional request body (for POST/PUT)."},
				},
				"required": []any{"url"},
			},
		},
		Invoke: func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				URL     string            `json:"url"`
				Method  string            `json:"method"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "error: invalid tool arguments", nil
			}
			if strings.TrimSpace(a.URL) == "" {
				return "error: url is required", nil
			}
			res, err := funcexec.Fetch(a.URL, funcsandbox.FetchOptions{Method: a.Method, Headers: a.Headers, Body: a.Body})
			if err != nil {
				return "error: " + err.Error(), nil
			}
			b, err := json.Marshal(map[string]any{"status": res.Status, "headers": res.Headers, "body": res.Body})
			if err != nil {
				return "error: could not encode response", nil
			}
			return string(b), nil
		},
	}
}

// parseFunctionAllowlist decodes the stored JSON array of function paths,
// tolerating an empty column.
func parseFunctionAllowlist(stored string) []string {
	if strings.TrimSpace(stored) == "" {
		return nil
	}
	var paths []string
	if err := json.Unmarshal([]byte(stored), &paths); err != nil {
		return nil
	}
	return paths
}

// agentFunctionTool wraps one manage_function endpoint as an agent tool. The
// function is loaded and run per call through the same funcexec path the HTTP
// endpoint uses, so its sandbox, secrets allowlist, and declared-write policy are
// identical whether a browser or the agent invokes it.
func (h *Handler) agentFunctionTool(projectID int, projectDB *db.ProjectDB, fnPath string, user map[string]any) agentrt.Tool {
	desc := fmt.Sprintf("Call the app's %q function endpoint. Pass the JSON body it expects in `body`.", fnPath)
	if fn, err := h.loadFunctionEndpoint(projectDB.Reader(), fnPath); err == nil && fn.Description != "" {
		desc = fmt.Sprintf("%s (function %q). Pass the JSON body it expects in `body`.", strings.TrimSuffix(fn.Description, "."), fnPath)
	}
	return agentrt.Tool{
		Def: llm.ToolDef{
			Name:        toolNameForFunction(fnPath),
			Description: desc,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"body": map[string]any{"type": "object", "description": "JSON body sent to the function as req.body."},
				},
			},
		},
		Invoke: func(ctx context.Context, argsJSON string) (string, error) {
			fn, err := h.loadFunctionEndpoint(projectDB.Reader(), fnPath)
			if err != nil {
				return fmt.Sprintf("error: function %q is not available", fnPath), nil
			}
			var a struct {
				Body any `json:"body"`
			}
			_ = json.Unmarshal([]byte(argsJSON), &a)
			reqObj := map[string]any{
				"method":  http.MethodPost,
				"path":    "/api/" + fnPath,
				"query":   map[string]any{},
				"headers": map[string]any{},
				"body":    a.Body,
				"user":    user,
			}
			spec := funcexec.Spec{Code: fn.Code, SecretsJSON: fn.Secrets}
			if fn.TimeoutMS > 0 {
				spec.Timeout = time.Duration(fn.TimeoutMS) * time.Millisecond
			}
			rt := funcexec.NewRuntime(projectID, projectDB.Reader(), projectDB.Writer(), h.deps.completeDeps())
			resp, err := funcexec.Execute(rt, spec, reqObj)
			if err != nil {
				// Hand a generic failure back in-band so the model can react without
				// leaking internal detail; the specifics go to the log.
				slog.Warn("agent function tool errored", "path", fnPath, "error", err)
				return "error: the function call failed", nil
			}
			return string(resp.Body), nil
		},
	}
}

// maxAgentTableNames bounds the table list put in the query_data description.
const maxAgentTableNames = 40

// agentTableNames lists the app's own tables (never the platform's of_* or
// sqlite_* ones) so the agent knows what it can query. Nil on any error.
func agentTableNames(projectDB *db.ProjectDB) []string {
	rows, err := projectDB.Reader().Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'of\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name LIMIT ?`,
		maxAgentTableNames)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// agentScopedTables returns the lower-cased names of tables behind an endpoint that
// scopes rows by owner or tenant. A read error answers nil with ok=false so the
// caller can fail closed.
func agentScopedTables(projectDB *db.ProjectDB) (map[string]bool, bool) {
	rows, err := projectDB.Reader().Query(
		`SELECT table_name FROM of_api_endpoints WHERE COALESCE(owner_column,'') != '' OR COALESCE(tenant_column,'') != ''`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil && n != "" {
			out[strings.ToLower(n)] = true
		}
	}
	if rows.Err() != nil {
		return nil, false
	}
	return out, true
}

var sqlIdentTokenRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// sqlMentionsTable reports whether any identifier token of the SQL equals one of the
// (lower-cased) table names. Quoting does not hide a name, because quotes are not
// identifier characters; a name inside a string literal is a harmless false positive.
func sqlMentionsTable(sqlText string, tables map[string]bool) (string, bool) {
	for _, tok := range sqlIdentTokenRe.FindAllString(sqlText, -1) {
		if lt := strings.ToLower(tok); tables[lt] {
			return lt, true
		}
	}
	return "", false
}

// agentDataQueryTool exposes one read-only SELECT over the project tables,
// guarded exactly like a function handler's ctx.query (funcexec.QueryReadOnly).
//
// ctx.query runs author-written SQL that scopes itself on req.user.id; here the MODEL
// writes the SQL, so the caller's rows are not something it can be trusted to filter.
// Tables behind an owner- or tenant-scoped endpoint are therefore withheld unless the
// caller has full access (a project key or the admin role); the app exposes such data
// to an agent through a function that filters on req.user.
func agentDataQueryTool(projectDB *db.ProjectDB, fullAccess bool) agentrt.Tool {
	desc := "Run ONE read-only SQL SELECT over the app's tables and return the rows as JSON. No writes; system tables and PASSWORD/ENCRYPTED columns are refused. Use ? placeholders and pass their values in params."
	var scoped map[string]bool
	if !fullAccess {
		var ok bool
		if scoped, ok = agentScopedTables(projectDB); !ok {
			// Cannot tell which tables are scoped: withhold every table.
			scoped = nil
		}
	}
	denyAll := !fullAccess && scoped == nil
	if names := agentTableNames(projectDB); len(names) > 0 && !denyAll {
		visible := names[:0:0]
		for _, n := range names {
			if !scoped[strings.ToLower(n)] {
				visible = append(visible, n)
			}
		}
		if len(visible) > 0 {
			desc += " Tables: " + strings.Join(visible, ", ") + "."
		}
		if len(visible) < len(names) {
			desc += " Tables holding per-user or per-tenant rows are not available to this caller."
		}
	}
	return agentrt.Tool{
		Def: llm.ToolDef{
			Name:        "query_data",
			Description: desc,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql":    map[string]any{"type": "string", "description": "A single SELECT (or WITH ... SELECT / VALUES). Use ? placeholders, never string-interpolated values."},
					"params": map[string]any{"type": "array", "description": "Values for the ? placeholders, in order.", "items": map[string]any{}},
				},
				"required": []any{"sql"},
			},
		},
		Invoke: func(ctx context.Context, argsJSON string) (string, error) {
			var a struct {
				SQL    string `json:"sql"`
				Params []any  `json:"params"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "error: invalid tool arguments", nil
			}
			if denyAll {
				return "error: data queries are unavailable for this caller", nil
			}
			if !fullAccess {
				if name, hit := sqlMentionsTable(a.SQL, scoped); hit {
					return fmt.Sprintf("error: table %q holds per-user or per-tenant rows and cannot be queried here; use an app function for that data", name), nil
				}
			}
			rows, err := funcexec.QueryReadOnly(projectDB.Reader(), a.SQL, a.Params)
			if err != nil {
				// The guard's own message tells the model how to fix the query, so
				// pass it through rather than a generic error.
				return "error: " + err.Error(), nil
			}
			b, err := json.Marshal(rows)
			if err != nil {
				return "error: could not encode rows", nil
			}
			return string(b), nil
		},
	}
}

// toolNameForFunction is the shared sanitizer; see agentrt.ToolNameForFunction.
// manage_agent validates at create/update time that no two allowlisted paths
// collide under it.
func toolNameForFunction(path string) string {
	return agentrt.ToolNameForFunction(path)
}
