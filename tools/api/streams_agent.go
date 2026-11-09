package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/internal/agentrt"
	"openfabrica/internal/endpoints"
	"openfabrica/tools"
)

// Agent endpoint transport handlers for manage_agent. Tool registration and arg
// shape live in manage_agent.go; the runtime loop lives in internal/agentrt and
// the HTTP handler in server/public/agent_endpoint.go. These handlers only
// persist and read the of_agent_endpoints config rows, mirroring streams_llm.go.

// agentPathEndsInRun reports whether a normalized endpoint path ends in a "run"
// segment. Such a path serves the agent at /api/<path>/run/run, because the tool
// appends its own /run sub-route - almost always a naming slip (the path should
// name the resource, e.g. "support" -> /api/support/run).
func agentPathEndsInRun(path string) bool {
	seg := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		seg = path[i+1:]
	}
	return strings.EqualFold(seg, "run")
}

// functionsCSVToJSON turns the functions arg (a CSV of manage_function endpoint
// paths) into the JSON array stored on the row. Each path is normalised the same
// way the function endpoints themselves are, blanks are dropped, and an empty
// input yields "" (the column default, meaning "no function tools").
func functionsCSVToJSON(csv string) string {
	if strings.TrimSpace(csv) == "" {
		return ""
	}
	var paths []string
	for _, p := range strings.Split(csv, ",") {
		if n := tools.NormalizeEndpointPath(p); n != "" {
			paths = append(paths, n)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	b, err := json.Marshal(paths)
	if err != nil {
		return ""
	}
	return string(b)
}

// validateAgentFunctions rejects a functions CSV in which two paths sanitize to
// the same provider tool name (agentrt.ToolNameForFunction maps every character
// outside [A-Za-z0-9_-] to '_', so "orders/create" and "orders_create" collide).
// A collision would ship duplicate tool defs to the provider and make one of the
// functions unreachable. Returns nil when the CSV is clean.
func validateAgentFunctions(csv string) *tools.Result {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	byName := map[string]string{}
	for _, p := range strings.Split(csv, ",") {
		n := tools.NormalizeEndpointPath(p)
		if n == "" {
			continue
		}
		name := agentrt.ToolNameForFunction(n)
		if prev, ok := byName[name]; ok {
			if prev == n {
				return tools.Fail("functions lists %q twice - each function path may appear once", n)
			}
			return tools.Fail("function paths %q and %q both map to the agent tool name %q (path characters outside letters, digits, '_' and '-' become '_'), so the model could only ever call one of them. Rename one endpoint so the names differ.", prev, n, name)
		}
		byName[name] = n
	}
	return nil
}

// epCreateAgent persists a new agent endpoint from AgentCreate's typed args.
// Create-only: an existing path is a no-op, matching the LLM proxy.
func epCreateAgent(ctx *tools.ToolContext, cfg agentEndpointConfig) (*tools.Result, error) {
	requiresAuth := cfg.RequiresAuth != nil && *cfg.RequiresAuth
	allowDataQuery := cfg.AllowDataQuery != nil && *cfg.AllowDataQuery
	allowHTTPFetch := cfg.AllowHTTPFetch != nil && *cfg.AllowHTTPFetch
	dailyTokenBudget := 0
	if cfg.DailyTokenBudget != nil {
		dailyTokenBudget = *cfg.DailyTokenBudget
	}

	path, errRes := tools.ValidateAndNormalizeEndpointPath(cfg.Path)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	if agentPathEndsInRun(path) {
		return tools.Fail(`endpoint_path %q would serve the agent at /api/%s/run/run: this tool appends its own "/run" sub-route, so a path ending in "run" doubles it. Name the path for the RESOURCE, not the action - e.g. "support" serves the agent at /api/support/run.`, path, path), nil
	}
	if rej := validateAgentFunctions(cfg.Functions); rej != nil {
		return rej, nil
	}

	var existingCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_agent_endpoints WHERE path = ?", path).Scan(&existingCount); err == nil && existingCount > 0 {
		return tools.Ok(map[string]any{
			"endpoint_path":   path,
			"already_existed": true,
			"message":         "Agent endpoint /api/" + path + " already exists, so nothing was created and NO option in this call was applied. Change its config with manage_agent action=update.",
		}), nil
	}

	maxTokens := 4096
	if cfg.MaxTokens > 0 {
		maxTokens = cfg.MaxTokens
	}
	temperature := 0.7
	if cfg.Temperature != 0 {
		temperature = cfg.Temperature
	}
	maxSteps := 8
	if cfg.MaxSteps > 0 {
		maxSteps = cfg.MaxSteps
	}
	rateLimit := 10
	if cfg.RateLimit > 0 {
		rateLimit = cfg.RateLimit
	}
	maxDepth := 1
	if cfg.MaxDepth > 0 {
		maxDepth = cfg.MaxDepth
	}
	maxSubagents := 0 // default: sub-agents disabled
	if cfg.MaxSubagents != nil {
		maxSubagents = *cfg.MaxSubagents
	}
	functions := functionsCSVToJSON(cfg.Functions)

	if err := ClaimEndpoint(ctx, endpoints.KindAgent, path, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(
		`INSERT INTO of_agent_endpoints (path, system_prompt, model_id, max_tokens, temperature, max_steps, rate_limit, daily_token_budget, requires_auth, cors_origins, functions, allow_data_query, max_subagents, max_depth, allow_http_fetch)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   system_prompt = excluded.system_prompt,
		   model_id = excluded.model_id,
		   max_tokens = excluded.max_tokens,
		   temperature = excluded.temperature,
		   max_steps = excluded.max_steps,
		   rate_limit = excluded.rate_limit,
		   daily_token_budget = excluded.daily_token_budget,
		   requires_auth = excluded.requires_auth,
		   cors_origins = excluded.cors_origins,
		   functions = excluded.functions,
		   allow_data_query = excluded.allow_data_query,
		   max_subagents = excluded.max_subagents,
		   max_depth = excluded.max_depth,
		   allow_http_fetch = excluded.allow_http_fetch`,
		path, cfg.SystemPrompt, cfg.ModelID, maxTokens, temperature, maxSteps, rateLimit, dailyTokenBudget, requiresAuth, cfg.CORSOrigins, functions, allowDataQuery, maxSubagents, maxDepth, allowHTTPFetch,
	)
	if err != nil {
		return nil, fmt.Errorf("creating agent endpoint: %w", err)
	}

	// The usage echo must match the endpoint's auth setting: with requires_auth
	// on (the default) a bare fetch gets 401.
	runFetch := "fetch"
	authNote := ""
	if requiresAuth {
		runFetch = "apiFetch"
		authNote = " (requires_auth is on: apiFetch from 'auth' attaches the token; a bare fetch gets 401)"
	}
	created := map[string]any{
		"endpoint_path":      path,
		"run_route":          fmt.Sprintf("POST /api/%s/run", path),
		"system_prompt":      cfg.SystemPrompt,
		"max_steps":          maxSteps,
		"max_tokens":         maxTokens,
		"temperature":        temperature,
		"rate_limit":         rateLimit,
		"daily_token_budget": dailyTokenBudget,
		"requires_auth":      requiresAuth,
		"functions":          functions,
		"allow_data_query":   allowDataQuery,
		"max_subagents":      maxSubagents,
		"max_depth":          maxDepth,
		"allow_http_fetch":   allowHTTPFetch,
		"run_usage":          "const res = await " + runFetch + "('/api/" + path + "/run', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({messages: [{role:'user', content:'...'}]})}); const data = await res.json(); // data.content is the agent's final answer; data.steps / data.stop_reason describe the run" + authNote,
		"budget_note":        "daily_token_budget caps the WHOLE run per UTC day (every step and, later, every sub-agent), not one call. Set it on any endpoint you expose; an agent spends more per request than a single completion.",
	}
	if missing := missingAgentFunctions(ctx, cfg.Functions); len(missing) > 0 {
		created["warning"] = missingFunctionsWarning(missing)
	}
	return tools.Ok(created), nil
}

// epUpdateAgent patches only the fields that were set, tri-state pointers letting
// update turn a setting off. Mirrors epUpdateLLM.
func epUpdateAgent(ctx *tools.ToolContext, cfg agentEndpointConfig) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(cfg.Path)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	setClauses := []string{}
	vals := []any{}
	if cfg.SystemPrompt != "" {
		setClauses = append(setClauses, "system_prompt = ?")
		vals = append(vals, cfg.SystemPrompt)
	}
	if cfg.ModelID != "" {
		setClauses = append(setClauses, "model_id = ?")
		vals = append(vals, cfg.ModelID)
	}
	if cfg.MaxTokens != 0 {
		setClauses = append(setClauses, "max_tokens = ?")
		vals = append(vals, cfg.MaxTokens)
	}
	if cfg.Temperature != 0 {
		setClauses = append(setClauses, "temperature = ?")
		vals = append(vals, cfg.Temperature)
	}
	if cfg.MaxSteps != 0 {
		setClauses = append(setClauses, "max_steps = ?")
		vals = append(vals, cfg.MaxSteps)
	}
	if cfg.RateLimit != 0 {
		setClauses = append(setClauses, "rate_limit = ?")
		vals = append(vals, cfg.RateLimit)
	}
	if cfg.DailyTokenBudget != nil {
		setClauses = append(setClauses, "daily_token_budget = ?")
		vals = append(vals, *cfg.DailyTokenBudget)
	}
	if cfg.RequiresAuth != nil {
		setClauses = append(setClauses, "requires_auth = ?")
		vals = append(vals, *cfg.RequiresAuth)
	}
	// "none" clears cors_origins / functions back to empty; an empty arg means "leave
	// unchanged", so without a sentinel these two could never be turned off.
	if cfg.CORSOrigins != "" {
		origins := cfg.CORSOrigins
		if strings.EqualFold(strings.TrimSpace(origins), "none") {
			origins = ""
		}
		setClauses = append(setClauses, "cors_origins = ?")
		vals = append(vals, origins)
	}
	var missingFunctions []string
	if cfg.Functions != "" {
		if strings.EqualFold(strings.TrimSpace(cfg.Functions), "none") {
			setClauses = append(setClauses, "functions = ?")
			vals = append(vals, "")
		} else {
			if rej := validateAgentFunctions(cfg.Functions); rej != nil {
				return rej, nil
			}
			setClauses = append(setClauses, "functions = ?")
			vals = append(vals, functionsCSVToJSON(cfg.Functions))
			missingFunctions = missingAgentFunctions(ctx, cfg.Functions)
		}
	}
	if cfg.AllowDataQuery != nil {
		setClauses = append(setClauses, "allow_data_query = ?")
		vals = append(vals, *cfg.AllowDataQuery)
	}
	if cfg.MaxSubagents != nil {
		setClauses = append(setClauses, "max_subagents = ?")
		vals = append(vals, *cfg.MaxSubagents)
	}
	if cfg.MaxDepth != 0 {
		setClauses = append(setClauses, "max_depth = ?")
		vals = append(vals, cfg.MaxDepth)
	}
	if cfg.AllowHTTPFetch != nil {
		setClauses = append(setClauses, "allow_http_fetch = ?")
		vals = append(vals, *cfg.AllowHTTPFetch)
	}

	if len(setClauses) == 0 {
		return tools.Fail("no fields to update"), nil
	}

	vals = append(vals, path)
	query := fmt.Sprintf("UPDATE of_agent_endpoints SET %s WHERE path = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("updating agent endpoint: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.Fail("agent endpoint '%s' not found", path), nil
	}
	out := map[string]any{"endpoint_path": path, "updated": true}
	if len(missingFunctions) > 0 {
		out["warning"] = missingFunctionsWarning(missingFunctions)
	}
	return tools.Ok(out), nil
}

// missingAgentFunctions returns the allowlisted paths that are not a function
// endpoint yet. The endpoint is still saved (functions may be created afterwards),
// but a path that never resolves only shows up at run time as a tool error string, so
// it is reported back at save time.
func missingAgentFunctions(ctx *tools.ToolContext, csv string) []string {
	var missing []string
	for _, p := range strings.Split(csv, ",") {
		n := tools.NormalizeEndpointPath(p)
		if n == "" {
			continue
		}
		var count int
		if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_function_endpoints WHERE path = ?", n).Scan(&count); err == nil && count == 0 {
			missing = append(missing, n)
		}
	}
	return missing
}

func missingFunctionsWarning(paths []string) string {
	return fmt.Sprintf("these `functions` paths are not manage_function endpoints yet: %s. The agent can only call functions that exist; create them (and spell the path exactly) or the model will get an \"is not available\" tool error.", strings.Join(paths, ", "))
}

func epListAgent(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT id, path, system_prompt, model_id, max_tokens, temperature, max_steps, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, requires_auth, cors_origins, functions, allow_data_query, max_subagents, max_depth, allow_http_fetch, created_at FROM of_agent_endpoints ORDER BY path")
	if err != nil {
		return nil, fmt.Errorf("listing agent endpoints: %w", err)
	}
	defer rows.Close()
	endpoints := []map[string]any{}
	for rows.Next() {
		var id int
		var path, systemPrompt, modelID, dayWindowStart, corsOrigins, functions, createdAt string
		var maxTokens, maxSteps, rateLimit, dailyTokenBudget, dailyTokensUsed, maxSubagents, maxDepth int
		var temperature float64
		var requiresAuth, allowDataQuery, allowHTTPFetch bool
		if err := rows.Scan(&id, &path, &systemPrompt, &modelID, &maxTokens, &temperature, &maxSteps, &rateLimit, &dailyTokenBudget, &dailyTokensUsed, &dayWindowStart, &requiresAuth, &corsOrigins, &functions, &allowDataQuery, &maxSubagents, &maxDepth, &allowHTTPFetch, &createdAt); err != nil {
			continue
		}
		endpoints = append(endpoints, map[string]any{
			"id": id, "endpoint_path": path, "system_prompt": systemPrompt, "model_id": modelID,
			"max_tokens": maxTokens, "temperature": temperature, "max_steps": maxSteps,
			"rate_limit": rateLimit, "requires_auth": requiresAuth, "cors_origins": corsOrigins,
			"functions": functions, "allow_data_query": allowDataQuery,
			"max_subagents":      maxSubagents,
			"max_depth":          maxDepth,
			"allow_http_fetch":   allowHTTPFetch,
			"daily_token_budget": dailyTokenBudget,
			"daily_tokens_used":  dailyTokensUsed,
			"day_window_start":   dayWindowStart,
			"run_route":          fmt.Sprintf("POST /api/%s/run", path),
			"created_at":         createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing agent endpoints: %w", err)
	}
	return tools.Ok(map[string]any{"endpoints": endpoints, "count": len(endpoints)}), nil
}

func epDeleteAgent(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_agent_endpoints", "agent endpoint", "manage_agent", "delete", endpointPath)
}
