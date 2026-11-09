package public

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/httpx"
	"openfabrica/internal/jsonrepair"
	"openfabrica/internal/projectmodel"
	"openfabrica/internal/replaymodel"
	"openfabrica/internal/stringutil"
	"openfabrica/providers/llm"
	"openfabrica/server/middleware"
)

// LLM endpoint handler

// llmEndpointConfig holds the config for an LLM endpoint.
type llmEndpointConfig struct {
	Path             string
	SystemPrompt     string
	ModelID          string
	MaxTokens        int
	Temperature      float64
	MaxHistory       int
	RateLimit        int
	DailyTokenBudget int
	DailyTokensUsed  int
	DayWindowStart   string
	RequiresAuth     bool
	CORSOrigins      string
	ResponseSchema   string // JSON Schema doc; empty = unconstrained free-form text
	KnowledgeBase    string // manage_knowledge base name; empty = plain chat (no retrieval)
}

func (h *Handler) loadLLMEndpoint(projectDB *sql.DB, path string) (*llmEndpointConfig, error) {
	var ep llmEndpointConfig
	err := projectDB.QueryRow(
		"SELECT path, system_prompt, model_id, max_tokens, temperature, max_history, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, requires_auth, cors_origins, response_schema, knowledge_base FROM of_llm_endpoints WHERE path = ?",
		path,
	).Scan(&ep.Path, &ep.SystemPrompt, &ep.ModelID, &ep.MaxTokens, &ep.Temperature, &ep.MaxHistory, &ep.RateLimit, &ep.DailyTokenBudget, &ep.DailyTokensUsed, &ep.DayWindowStart, &ep.RequiresAuth, &ep.CORSOrigins, &ep.ResponseSchema, &ep.KnowledgeBase)
	if err != nil {
		return nil, err
	}
	return &ep, nil
}

// checkAndResetDailyBudget rolls the per-endpoint UTC-day counter forward when
// a new day has started, then reports whether the budget still has headroom.
// Returns (allowed, currentUsed, err). budget==0 means unlimited; in that
// case allowed is always true and the counter is still rolled so list output
// reflects today's window. Mutates daily_tokens_used and day_window_start on
// the row whenever the window rolls, and archives the previous day's total
// into of_llm_usage_daily so manage_analytics can build a time series.
func checkAndResetDailyBudget(projectDB *sql.DB, ep *llmEndpointConfig) (bool, int, error) {
	today := time.Now().UTC().Format("2006-01-02")
	if ep.DayWindowStart != today {
		// Archive the previous day's total before resetting. Only meaningful
		// when there was a previous window AND the endpoint actually saw
		// traffic; INSERT OR IGNORE guards against double-archival if two
		// concurrent first-of-day requests race the reset (the second one
		// silently no-ops on the UNIQUE(path, day) constraint).
		if ep.DayWindowStart != "" && ep.DailyTokensUsed > 0 {
			if _, err := projectDB.Exec(
				"INSERT OR IGNORE INTO of_llm_usage_daily (path, day, tokens_used) VALUES (?, ?, ?)",
				ep.Path, ep.DayWindowStart, ep.DailyTokensUsed,
			); err != nil {
				slog.Warn("LLM endpoint: failed to archive previous day's usage", "path", ep.Path, "day", ep.DayWindowStart, "error", err)
			}
		}
		if _, err := projectDB.Exec(
			"UPDATE of_llm_endpoints SET daily_tokens_used = 0, day_window_start = ? WHERE path = ?",
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

// publicSpendAllowed applies the project's public AI pool and its dollar cap
// (llm.CheckPublicSpend) on top of an endpoint's own daily budget, for the LLM proxy
// and agent endpoints alike. When the project is over a limit it answers 429 and
// returns false.
func publicSpendAllowed(w http.ResponseWriter, reader *sql.DB, projectID int, path string) bool {
	st := llm.CheckPublicSpend(reader)
	if !st.Exceeded {
		return true
	}
	slog.Warn("AI endpoint refused: project daily AI limit reached", "project_id", projectID, "path", path, "limit", st.Reason)
	w.Header().Set("Retry-After", "86400")
	writeError(w, http.StatusTooManyRequests, "daily AI limit reached. Resets at 00:00 UTC.")
	return false
}

// recordLLMUsage increments the per-endpoint daily counter by the given token
// total. Best-effort: a failure here is logged but does not surface to the
// end user, who already got their successful completion.
func recordLLMUsage(projectDB *sql.DB, path string, tokens int) {
	if tokens <= 0 {
		return
	}
	if _, err := projectDB.Exec(
		"UPDATE of_llm_endpoints SET daily_tokens_used = daily_tokens_used + ? WHERE path = ?",
		tokens, path,
	); err != nil {
		slog.Warn("LLM endpoint: failed to record usage", "path", path, "tokens", tokens, "error", err)
	}
}

// llmRateLimiter tracks request counts per key for simple rate limiting. Keys
// include the client IP, so idle keys are swept at most once per window to keep
// the map from growing with every address that ever called.
var llmRateLimiter = struct {
	sync.Mutex
	counts    map[string][]time.Time
	lastSweep time.Time
}{counts: make(map[string][]time.Time)}

// sweepIdleRateLimitKeys drops keys whose newest request is older than window.
// Caller holds llmRateLimiter's lock.
func sweepIdleRateLimitKeys(window time.Time) {
	for k, ts := range llmRateLimiter.counts {
		if len(ts) == 0 || !ts[len(ts)-1].After(window) {
			delete(llmRateLimiter.counts, k)
		}
	}
}

// checkEndpointRateLimit returns true if the request is within the rate limit.
// A maxPerMinute of 0 (or less) disables the cap: an unconfigured limit must
// admit every request, not reject it (len(valid) >= 0 is always true).
func checkEndpointRateLimit(key string, maxPerMinute int) bool {
	if maxPerMinute <= 0 {
		return true
	}
	llmRateLimiter.Lock()
	defer llmRateLimiter.Unlock()

	now := time.Now()
	window := now.Add(-time.Minute)
	if now.Sub(llmRateLimiter.lastSweep) >= time.Minute {
		sweepIdleRateLimitKeys(window)
		llmRateLimiter.lastSweep = now
	}

	// Filter out expired entries.
	timestamps := llmRateLimiter.counts[key]
	valid := timestamps[:0]
	for _, ts := range timestamps {
		if ts.After(window) {
			valid = append(valid, ts)
		}
	}

	if len(valid) >= maxPerMinute {
		llmRateLimiter.counts[key] = valid
		return false
	}

	llmRateLimiter.counts[key] = append(valid, now)
	return true
}

// llmChatRequest is the JSON body for LLM chat/complete requests.
type llmChatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

const maxLLMBodySize = 64 * 1024 // 64 KB

// llmThinkingOff is the shared, addressable false the proxy points every
// completion's Thinking at, disabling extended reasoning on the public LLM
// endpoint. Mirrors engine/spec.go's thinkingOff: hidden chain-of-thought bills
// against the output budget and, on schema-constrained /complete calls, can
// exhaust it before the JSON is emitted. See the CompletionRequest in apiLLM.
var llmThinkingOff = false

// apiLLM handles POST /api/{path}/chat and /api/{path}/complete.
func (h *Handler) apiLLM(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *db.ProjectDB, endpointPath, mode string) {
	// The endpoint row is read from the read pool; the budget counter and the
	// call log are writes, so they take the single-connection writer.
	reader, writer := projectDB.Reader(), projectDB.Writer()

	ep, err := h.loadLLMEndpoint(reader, endpointPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "LLM endpoint not found")
		return
	}

	// Per-endpoint CORS on the actual response. The OPTIONS preflight is answered
	// centrally upstream by handleAPIPreflight, so this handler only ever runs for
	// the real method.
	if ep.CORSOrigins != "" {
		applyEndpointCORS(w, ep.CORSOrigins, r.Header.Get("Origin"), "POST, OPTIONS", "Content-Type, Authorization")
	}

	// Auth check.
	if ep.RequiresAuth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !h.validateProjectTokenOrJWT(project.ID, token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	// Rate limit by IP. middleware.ClientIP only trusts X-Forwarded-For from a
	// loopback peer (the public HTTPS listener proxies over loopback), so a
	// remote client can't spoof the header to dodge the per-IP limit.
	rateLimitKey := fmt.Sprintf("%d:%s:%s", project.ID, ep.Path, middleware.RateLimitIdentity(r))
	if !checkEndpointRateLimit(rateLimitKey, ep.RateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	// Daily-token budget gate. Rolls the window forward on the first hit of
	// each UTC day before checking. A zero budget means unlimited, so the
	// check is a no-op for endpoints that haven't opted in.
	allowed, used, err := checkAndResetDailyBudget(writer, ep)
	if err != nil {
		slog.Error("LLM endpoint: budget check failed", "project_id", project.ID, "path", ep.Path, "error", err)
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

	// Parse request body.
	var req llmChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxLLMBodySize)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required and must not be empty")
		return
	}

	// Validate and cap messages, only allow user/assistant roles, cap at max_history.
	var messages []llm.Message
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue // silently drop system or other roles
		}
		messages = append(messages, llm.Message{Role: llm.Role(m.Role), Content: m.Content})
	}
	if len(messages) > ep.MaxHistory {
		messages = messages[len(messages)-ep.MaxHistory:]
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != llm.RoleUser {
		writeError(w, http.StatusBadRequest, "last message must be from user")
		return
	}

	// Resolve LLM provider.
	provider, modelID, err := h.resolveLLMProvider(project.ID)
	if err != nil {
		slog.Error("LLM endpoint: failed to resolve provider", "project_id", project.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "AI service not available")
		return
	}

	// Log the call (source="proxy") so a live-mode reply is inspectable after the
	// fact the way an engine call is: without this the only trace of the busiest
	// LLM surface in the platform is the daily token counter. The endpoint path
	// goes in session_id, which is what identifies a call's origin here. These rows
	// are the public pool (llm.PublicSources) the gate above measures.
	llmLogger := llm.NewDBLLMLogger(writer)
	defer llmLogger.Close()
	provider = llm.NewLoggedProvider(provider, llmLogger, "proxy", ep.Path, nil, nil)

	// Override model if endpoint specifies one.
	if ep.ModelID != "" {
		modelID = ep.ModelID
	}

	// Retrieval-augmented generation: when the endpoint is bound to a knowledge
	// base, retrieve the most relevant chunks for the latest user message and
	// ground the system prompt in them. Best-effort - a retrieval miss degrades
	// to plain chat rather than failing the request.
	systemPrompt := ep.SystemPrompt
	if ep.KnowledgeBase != "" {
		lastUser := messages[len(messages)-1].Content
		if ctxBlock := h.retrieveKnowledgeContext(r.Context(), project.ID, reader, ep.KnowledgeBase, lastUser, 5); ctxBlock != "" {
			systemPrompt = strings.TrimSpace(systemPrompt + "\n\n" +
				"Answer using ONLY the context below. If the answer is not in the context, say you don't know.\n\n" +
				"--- CONTEXT ---\n" + ctxBlock + "\n--- END CONTEXT ---")
		}
	}

	temp := ep.Temperature
	completionReq := llm.CompletionRequest{
		Model:       modelID,
		System:      systemPrompt,
		Messages:    messages,
		MaxTokens:   ep.MaxTokens,
		Temperature: &temp,
		// The configured system prompt is identical across an endpoint's calls,
		// so cache it - except when a knowledge base splices per-request RAG
		// context into it, where a cache write could never be re-read. The key
		// routes all of one endpoint's calls to the same cache node; transports
		// with automatic caching ignore CacheSystem.
		CacheSystem:    ep.KnowledgeBase == "",
		PromptCacheKey: fmt.Sprintf("proxy:%d:%s", project.ID, ep.Path),
		// Disable extended reasoning, matching every operational LLM turn in the engine
		// (engine/spec.go thinkingOff). Reasoning models bill hidden chain-of-thought
		// against the output budget: on /complete it can exhaust max_tokens before the
		// schema-constrained JSON is emitted (truncated reply -> schema mismatch -> 502),
		// and on /chat the proxy never forwards the reasoning channel anyway. The proxy
		// exposes no way to surface reasoning, so leaving it on never helps here.
		Thinking: &llmThinkingOff,
	}

	// Native structured output: when the endpoint declares a JSON schema, force the
	// provider to emit conforming JSON so the client's JSON.parse can't fail on stray
	// prose or fences. Only on /complete - the /chat token stream is for free-form UIs
	// that don't parse the reply as one JSON object. An invalid stored schema falls
	// through to free-form text via the unsupported-schema path below.
	//
	// schemaDoc is the rail-pinned form of the stored schema, shown to both the provider
	// and the corrective retry so the shape asked for is the same in both places.
	schemaDoc := pinSchemaRails(ep.ResponseSchema)
	if mode == "complete" && schemaDoc != "" {
		// Only pin the schema when the provider advertises structured output, so
		// providers that don't support it skip a guaranteed-failing first call and
		// fall back to the system-prompt instruction. The CodeSchemaUnsupported
		// retry below still covers model-level rejection on providers that claim
		// support at the transport level.
		if sop, ok := provider.(llm.StructuredOutputProvider); ok && sop.SupportsStructuredOutput() {
			completionReq.ResponseSchema = &llm.ResponseSchema{
				Name:   "output",
				Schema: json.RawMessage(schemaDoc),
				// Pin strict mode only when the schema satisfies OpenAI's strict
				// rules (additionalProperties:false + all properties required at
				// every level), so providers that truly constrain decoding are
				// forced to conform. Non-compatible schemas stay non-strict to
				// avoid an outright provider reject.
				Strict: schemaIsStrictCompatible(schemaDoc),
			}
		}
	}

	if mode == "complete" {
		// Non-streaming: return full response as JSON.
		resp, err := provider.Complete(r.Context(), completionReq)
		// If the provider/model can't honour the JSON schema, retry once without
		// it rather than failing - the system prompt still asks for JSON, so a
		// capable model usually still complies. Keeps weaker providers working.
		if err != nil {
			if e, ok := llm.AsLLMError(err); ok && e.Code == llm.CodeSchemaUnsupported && completionReq.ResponseSchema != nil {
				completionReq.ResponseSchema = nil
				resp, err = provider.Complete(r.Context(), completionReq)
			}
		}
		if err != nil {
			slog.Error("LLM endpoint: completion failed", "project_id", project.ID, "path", ep.Path, "error", err)
			writeError(w, http.StatusInternalServerError, "AI completion failed")
			return
		}
		recordLLMUsage(writer, ep.Path, resp.Usage.InputTokens+resp.Usage.OutputTokens)
		// A reply cut off at the token cap is incomplete - and unparseable when
		// the client expects JSON. Surface that distinctly instead of returning
		// broken content the client would report as "invalid JSON".
		if resp.StopReason == "max_tokens" {
			slog.Warn("LLM endpoint: completion truncated at max_tokens", "project_id", project.ID, "path", ep.Path, "max_tokens", ep.MaxTokens)
			truncatedMaxTokensErr(w, ep.MaxTokens)
			return
		}

		// Structured-output enforcement. On providers that don't truly constrain
		// decoding (several OpenAI-compatible gateways treat response_format as a hint),
		// a weak model can return the wrong shape - classically a bare array when an
		// object was requested. When a schema is declared, repair the loose-JSON
		// artifacts weak models emit (stray fence, Python True/False/None, trailing
		// comma), verify the top-level shape, retry once on a mismatch with the schema
		// spelled out, then fail with a clear 502 rather than passing content the client
		// would choke on in JSON.parse.
		if schemaDoc != "" {
			resp.Content = jsonrepair.Repair(resp.Content)
			if !structuralSchemaCheck(resp.Content, schemaDoc) {
				slog.Warn("LLM endpoint: response did not match schema; retrying once",
					append([]any{"project_id", project.ID, "path", ep.Path}, schemaMissDetail(resp, completionReq.ResponseSchema)...)...)
				// Quote how the rejected reply opened. Naming the actual mistake
				// beats an abstract "did not match", and a sample is enough: the
				// full reply is a whole document, and re-sending it would double
				// the input of a call that is already a second generation.
				corrective := completionReq
				corrective.Messages = append(append([]llm.Message{}, completionReq.Messages...), llm.Message{
					Role: llm.RoleUser,
					Content: "Your previous reply did not match the required JSON shape. It started with: " +
						logSnippet(resp.Content, 400) +
						"\n\nReply with ONLY a single JSON value that conforms exactly to this JSON schema - no prose, no markdown fences, and not a bare array unless the schema's top-level type is \"array\":\n" + schemaDoc,
				})
				if retryResp, retryErr := provider.Complete(r.Context(), corrective); retryErr == nil {
					recordLLMUsage(writer, ep.Path, retryResp.Usage.InputTokens+retryResp.Usage.OutputTokens)
					if retryResp.StopReason == "max_tokens" {
						slog.Warn("LLM endpoint: schema-retry truncated at max_tokens", "project_id", project.ID, "path", ep.Path, "max_tokens", ep.MaxTokens)
						writeError(w, http.StatusBadGateway,
							fmt.Sprintf("output truncated at max_tokens (%d); increase the endpoint's max_tokens or shorten the request", ep.MaxTokens))
						return
					}
					retryResp.Content = jsonrepair.Repair(retryResp.Content)
					resp = retryResp
				}
				if !structuralSchemaCheck(resp.Content, schemaDoc) {
					slog.Warn("LLM endpoint: response did not match schema after retry",
						append([]any{"project_id", project.ID, "path", ep.Path}, schemaMissDetail(resp, completionReq.ResponseSchema)...)...)
					writeError(w, http.StatusBadGateway,
						"the AI response did not match the required JSON shape; please try again")
					return
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     resp.Content,
			"model":       resp.Model,
			"stop_reason": resp.StopReason,
			"usage": map[string]int{
				"input_tokens":  resp.Usage.InputTokens,
				"output_tokens": resp.Usage.OutputTokens,
			},
		})
		return
	}

	// Streaming: SSE response.
	sse, ok := httpx.NewSSE(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	err = provider.Stream(r.Context(), completionReq, func(chunk llm.StreamChunk) {
		if chunk.Error != nil {
			sseEvent(sse, "error", map[string]string{"error": chunk.Error.Error()})
			return
		}
		if chunk.Delta != "" {
			sseEvent(sse, "token", map[string]string{"text": chunk.Delta})
		}
		if chunk.Done {
			doneData := map[string]any{}
			if chunk.StopReason != "" {
				doneData["stop_reason"] = chunk.StopReason
			}
			if chunk.Usage != nil {
				doneData["usage"] = map[string]int{
					"input_tokens":  chunk.Usage.InputTokens,
					"output_tokens": chunk.Usage.OutputTokens,
				}
				recordLLMUsage(writer, ep.Path, chunk.Usage.InputTokens+chunk.Usage.OutputTokens)
			}
			sseEvent(sse, "done", doneData)
		}
	})
	if err != nil {
		slog.Error("LLM endpoint: stream failed", "project_id", project.ID, "path", ep.Path, "error", err)
		sseEvent(sse, "error", map[string]string{"error": "stream failed"})
	}
}

// schemaMissDetail returns the slog fields that say why a reply failed the
// shape check: what the model actually sent, how much of it there was, why it
// stopped, and whether the schema was enforced on the wire at all. Without
// these a schema miss is indistinguishable from any other, and the reply itself
// is gone by the time anyone reads the log.
func schemaMissDetail(resp *llm.CompletionResponse, sent *llm.ResponseSchema) []any {
	strict := false
	if sent != nil {
		strict = sent.Strict
	}
	return []any{
		"stop_reason", resp.StopReason,
		"content_bytes", len(resp.Content),
		"output_tokens", resp.Usage.OutputTokens,
		"schema_enforced", sent != nil,
		"strict", strict,
		"content_head", logSnippet(resp.Content, 200),
	}
}

// logSnippet returns at most n runes of s collapsed onto one line, for carrying
// a sample of a model reply in a log field or a prompt without flooding either.
func logSnippet(s string, n int) string {
	return stringutil.Truncate(strings.Join(strings.Fields(s), " "), n)
}

// sseEvent JSON-encodes data into one SSE frame on the prepared writer.
func sseEvent(sse *httpx.SSEWriter, event string, data any) {
	_ = sse.Event(event, []byte(mustJSON(data)))
}

// truncatedMaxTokensErr writes the 502 returned when a completion is cut off at
// the token cap, telling the caller how to raise the ceiling.
func truncatedMaxTokensErr(w http.ResponseWriter, maxTokens int) {
	writeError(w, http.StatusBadGateway,
		fmt.Sprintf("output truncated at max_tokens (%d); increase the endpoint's max_tokens or shorten the request", maxTokens))
}

// resolveLLMProvider resolves the project's model (projectmodel.Resolve, the engine's
// resolver: the project's own model, else the instance default, on the shared LLM
// HTTP client). While a user-flow replay of the project runs, its model calls are
// answered by a stand-in: instant, free and the same every time.
func (h *Handler) resolveLLMProvider(projectID int) (llm.Provider, string, error) {
	if h.deps.DB == nil {
		return nil, "", fmt.Errorf("no global database")
	}
	return replaymodel.Resolve(projectID, func() (llm.Provider, string, error) {
		if h.resolveModel != nil {
			return h.resolveModel(projectID)
		}
		res, err := projectmodel.Resolve(h.deps.DB.DB, h.deps.Encryptor, h.deps.LLMHTTPClient, projectID)
		return res.Provider, res.Model, err
	})
}

// mustJSON marshals v to JSON string, returning "{}" on error.
