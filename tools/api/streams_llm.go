package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/internal/endpoints"
	"openfabrica/tools"
)

// encodeResponseSchema turns the response_schema arg (a JSON object the model
// passed, or nil) into the string stored on the endpoint row. A nil map yields
// ("", false) meaning "not provided"; a non-nil but empty {} yields ("", true)
// meaning "explicitly cleared"; a populated object yields its compact JSON.
func encodeResponseSchema(m map[string]any) (string, bool) {
	if m == nil {
		return "", false
	}
	if len(m) == 0 {
		return "", true
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", true
	}
	return string(b), true
}

// LLM proxy transport handlers for manage_llm_proxy. Tool registration and
// arg shape live in manage_llm_proxy.go.

// LLM proxy endpoint actions

// epCreateLLM takes typed args straight from LLMProxyCreate. Zero-valued
// optional numerics fall back to their documented defaults, matching the prior
// merge-map shape (the merge helper dropped zero values, so the handler's
// defaults applied).
func epCreateLLM(ctx *tools.ToolContext, cfg llmEndpointConfig) (*tools.Result, error) {
	// Unpack the config into the local names the handler body uses. requiresAuth
	// collapses the tri-state pointer to a plain bool: create always supplies a
	// non-nil value, so a nil (defensively) means the documented default, false.
	rawPath := cfg.Path
	requiresAuth := cfg.RequiresAuth != nil && *cfg.RequiresAuth
	rateLimitArg := cfg.RateLimit
	dailyTokenBudgetArg := 0
	if cfg.DailyTokenBudget != nil {
		dailyTokenBudgetArg = *cfg.DailyTokenBudget
	}
	modelID := cfg.ModelID
	systemPrompt := cfg.SystemPrompt
	maxHistoryArg := cfg.MaxHistory
	maxTokensArg := cfg.MaxTokens
	temperatureArg := cfg.Temperature
	corsOrigins := cfg.CORSOrigins
	responseSchemaArg := cfg.ResponseSchema
	knowledgeBase := cfg.KnowledgeBase

	path, errRes := tools.ValidateAndNormalizeEndpointPath(rawPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	// Skip if LLM endpoint already exists (prevents duplicate creation during BUILD).
	var existingCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_llm_endpoints WHERE path = ?", path).Scan(&existingCount); err == nil && existingCount > 0 {
		return tools.Ok(map[string]any{
			"endpoint_path":   path,
			"already_existed": true,
			"message":         "LLM endpoint /api/" + path + " already exists, so nothing was created and NO option in this call was applied (model, system_prompt, budget included). Change its config with manage_llm_proxy action=update.",
		}), nil
	}

	responseSchema, _ := encodeResponseSchema(responseSchemaArg)
	// A JSON-wrapped reply (code/markup escaped inside a JSON envelope) is large,
	// and any truncation makes it unparseable. So when the endpoint constrains
	// output to a schema, the default response budget floor is higher.
	maxTokens := 4096
	if responseSchema != "" {
		maxTokens = 8192
	}
	if maxTokensArg > 0 {
		maxTokens = maxTokensArg
	}
	temperature := 0.7
	if temperatureArg != 0 {
		temperature = temperatureArg
	}
	maxHistory := 20
	if maxHistoryArg > 0 {
		maxHistory = maxHistoryArg
	}
	rateLimit := 10
	if rateLimitArg > 0 {
		rateLimit = rateLimitArg
	}
	// Negatives are rejected by the daily_token_budget min:"0" tag before dispatch.
	dailyTokenBudget := dailyTokenBudgetArg

	if err := ClaimEndpoint(ctx, endpoints.KindLLM, path, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(
		`INSERT INTO of_llm_endpoints (path, system_prompt, model_id, max_tokens, temperature, max_history, rate_limit, daily_token_budget, requires_auth, cors_origins, response_schema, knowledge_base)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   system_prompt = excluded.system_prompt,
		   model_id = excluded.model_id,
		   max_tokens = excluded.max_tokens,
		   temperature = excluded.temperature,
		   max_history = excluded.max_history,
		   rate_limit = excluded.rate_limit,
		   daily_token_budget = excluded.daily_token_budget,
		   requires_auth = excluded.requires_auth,
		   cors_origins = excluded.cors_origins,
		   response_schema = excluded.response_schema,
		   knowledge_base = excluded.knowledge_base`,
		path, systemPrompt, modelID, maxTokens, temperature, maxHistory, rateLimit, dailyTokenBudget, requiresAuth, corsOrigins, responseSchema, knowledgeBase,
	)
	if err != nil {
		return nil, fmt.Errorf("creating LLM endpoint: %w", err)
	}

	// Same swap as the agent endpoint's usage hint: on requires_auth a bare fetch 401s,
	// so the copy-paste snippet uses apiFetch from 'auth', which adds the header.
	callFetch := "fetch"
	if requiresAuth {
		callFetch = "apiFetch"
	}
	out := map[string]any{
		"endpoint_path":      path,
		"knowledge_base":     knowledgeBase,
		"chat_route":         fmt.Sprintf("POST /api/%s/chat", path),
		"complete_route":     fmt.Sprintf("POST /api/%s/complete", path),
		"system_prompt":      systemPrompt,
		"max_tokens":         maxTokens,
		"temperature":        temperature,
		"max_history":        maxHistory,
		"rate_limit":         rateLimit,
		"daily_token_budget": dailyTokenBudget,
		"requires_auth":      requiresAuth,
		"response_schema":    responseSchema,
		// usage_stream buffers ACROSS reads (a frame routinely straddles two chunks, and
		// parsing each chunk on its own drops it) and dispatches on the SSE event NAME,
		// because the body is JSON - appending the raw data line renders {"text":"..."}
		// into the page instead of the words.
		"usage_stream":   "const res = await " + callFetch + "('/api/" + path + "/chat', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({messages: [{role:'user', content:'Hello'}]})}); const reader = res.body.getReader(); const dec = new TextDecoder(); let buf = ''; for (;;) { const {value, done} = await reader.read(); if (done) break; buf += dec.decode(value, {stream:true}); const frames = buf.split('\n\n'); buf = frames.pop(); for (const f of frames) { let name = 'message', data = ''; for (const line of f.split('\n')) { if (line.startsWith('event: ')) name = line.slice(7).trim(); else if (line.startsWith('data: ')) data += line.slice(6); } if (!data) continue; let p; try { p = JSON.parse(data); } catch (e) { continue; } if (name === 'token') output.textContent += p.text; else if (name === 'done') { /* final frame: p.stop_reason, p.usage - stream closes next */ } else if (name === 'error') showError(p.error); } }",
		"usage_complete": "const res = await " + callFetch + "('/api/" + path + "/complete', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({messages: [{role:'user', content:'Hello'}]})}); const data = await res.json(); // data.content holds the assistant reply",
		"sse_format":     "SSE events (NAMED; there is no [DONE] sentinel): 'token' data {\"text\":\"chunk\"} - append parsed.text; 'done' data {\"stop_reason\":...,\"usage\":{...}} - the LAST frame, stream closes after it; 'error' data {\"error\":\"msg\"} - an in-band failure on HTTP 200, so render an error state, never as text.",
		"auth_note":      "requires_auth on /chat and /complete reads the Authorization header ONLY - there is no ?token= query fallback (unlike an SSE stream or a WebSocket). Use apiFetch from 'auth'.",
	}
	if !requiresAuth && dailyTokenBudget == 0 {
		out["warning"] = "This endpoint is public and has NO daily token budget: any visitor can spend the project's LLM provider quota (rate_limit only caps one IP). Set daily_token_budget, or requires_auth=true, unless that exposure is intended."
	}
	return tools.Ok(out), nil
}

// epUpdateLLM takes typed args straight from LLMProxyUpdate. Each field is
// patched only when set (non-empty string / non-zero numeric), matching the
// prior merge-map shape where the absent key skipped the column.
// requires_auth is a tri-state pointer so `requires_auth: false` can reopen
// an over-gated endpoint - a plain bool could never express false.
func epUpdateLLM(ctx *tools.ToolContext, cfg llmEndpointConfig) (*tools.Result, error) {
	// Unpack the config into the local names the handler body uses. requiresAuth
	// stays a tri-state pointer here: update patches it only when non-nil, so
	// `requires_auth: false` can reopen an over-gated endpoint.
	rawPath := cfg.Path
	requiresAuth := cfg.RequiresAuth
	rateLimit := cfg.RateLimit
	dailyTokenBudget := cfg.DailyTokenBudget
	modelID := cfg.ModelID
	systemPrompt := cfg.SystemPrompt
	maxHistory := cfg.MaxHistory
	maxTokens := cfg.MaxTokens
	temperature := cfg.Temperature
	corsOrigins := cfg.CORSOrigins
	responseSchemaArg := cfg.ResponseSchema
	knowledgeBase := cfg.KnowledgeBase

	path := tools.NormalizeEndpointPath(rawPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	setClauses := []string{}
	vals := []any{}

	if systemPrompt != "" {
		setClauses = append(setClauses, "system_prompt = ?")
		vals = append(vals, systemPrompt)
	}
	if modelID != "" {
		setClauses = append(setClauses, "model_id = ?")
		vals = append(vals, modelID)
	}
	if maxTokens != 0 {
		setClauses = append(setClauses, "max_tokens = ?")
		vals = append(vals, maxTokens)
	}
	if temperature != 0 {
		setClauses = append(setClauses, "temperature = ?")
		vals = append(vals, temperature)
	}
	if maxHistory != 0 {
		setClauses = append(setClauses, "max_history = ?")
		vals = append(vals, maxHistory)
	}
	if rateLimit != 0 {
		setClauses = append(setClauses, "rate_limit = ?")
		vals = append(vals, rateLimit)
	}
	if dailyTokenBudget != nil {
		setClauses = append(setClauses, "daily_token_budget = ?")
		vals = append(vals, *dailyTokenBudget)
	}
	if requiresAuth != nil {
		setClauses = append(setClauses, "requires_auth = ?")
		vals = append(vals, *requiresAuth)
	}
	if corsOrigins != "" {
		setClauses = append(setClauses, "cors_origins = ?")
		vals = append(vals, corsOrigins)
	}
	// Only patch response_schema when the arg was actually present (encode's
	// second return). An empty {} clears it; a populated object replaces it.
	if rs, present := encodeResponseSchema(responseSchemaArg); present {
		setClauses = append(setClauses, "response_schema = ?")
		vals = append(vals, rs)
	}
	if knowledgeBase != "" {
		// "none" unbinds the endpoint back to plain chat; any other value rebinds.
		if strings.EqualFold(knowledgeBase, "none") {
			knowledgeBase = ""
		}
		setClauses = append(setClauses, "knowledge_base = ?")
		vals = append(vals, knowledgeBase)
	}

	if len(setClauses) == 0 {
		return tools.Fail("no fields to update"), nil
	}

	vals = append(vals, path)
	query := fmt.Sprintf("UPDATE of_llm_endpoints SET %s WHERE path = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("updating LLM endpoint: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.Fail("LLM endpoint '%s' not found", path), nil
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "updated": true}), nil
}

func epListLLM(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT id, path, system_prompt, model_id, max_tokens, temperature, max_history, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, requires_auth, cors_origins, response_schema, COALESCE(knowledge_base, ''), created_at FROM of_llm_endpoints ORDER BY path")
	if err != nil {
		return nil, fmt.Errorf("listing LLM endpoints: %w", err)
	}
	defer rows.Close()
	// Non-nil so an empty result serialises as [] rather than a bare null.
	endpoints := []map[string]any{}
	for rows.Next() {
		var id int
		var path, systemPrompt, modelID, corsOrigins, dayWindowStart, responseSchema, knowledgeBase string
		var maxTokens, maxHistory, rateLimit, dailyTokenBudget, dailyTokensUsed int
		var temperature float64
		var requiresAuth bool
		var createdAt string
		if err := rows.Scan(&id, &path, &systemPrompt, &modelID, &maxTokens, &temperature, &maxHistory, &rateLimit, &dailyTokenBudget, &dailyTokensUsed, &dayWindowStart, &requiresAuth, &corsOrigins, &responseSchema, &knowledgeBase, &createdAt); err != nil {
			continue
		}
		endpoints = append(endpoints, map[string]any{
			"id": id, "endpoint_path": path, "system_prompt": systemPrompt, "model_id": modelID,
			"max_tokens": maxTokens, "temperature": temperature, "max_history": maxHistory,
			"rate_limit": rateLimit, "requires_auth": requiresAuth, "cors_origins": corsOrigins,
			"response_schema":    responseSchema,
			"knowledge_base":     knowledgeBase,
			"daily_token_budget": dailyTokenBudget,
			"daily_tokens_used":  dailyTokensUsed,
			"day_window_start":   dayWindowStart,
			"chat_route":         fmt.Sprintf("POST /api/%s/chat", path),
			"complete_route":     fmt.Sprintf("POST /api/%s/complete", path),
			"created_at":         createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing LLM endpoints: %w", err)
	}
	return tools.Ok(map[string]any{
		"endpoints": endpoints,
		"count":     len(endpoints),
	}), nil
}

func epDeleteLLM(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_llm_endpoints", "LLM endpoint", "manage_llm_proxy", "delete", endpointPath)
}
