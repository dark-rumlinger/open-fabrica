package engine

// The single LLM call path. EVERY provider call in the engine goes through
// llmCaller.call, which layers, in order: replay cache read, concurrency limit,
// retry/fallback, call logging, replay cache write.
//
// Cache read discipline: a hash is served AT MOST ONCE per session segment so a
// deterministic failure can't replay in a loop, and never when the caller opts
// out (NoReplayCache) or the request carries images. A write means only that the
// transport returned - a caller that could not use the body evicts it with
// cacheDiscard.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"openfabrica/db"
	"openfabrica/internal/sqliteretry"
	"openfabrica/providers/llm"
)

// llmCaller executes provider calls for one project worker.
type llmCaller struct {
	limiter *llmLimiter
	pdb     *db.ProjectDB
	dbLog   llm.LLMLogger
	logger  *slog.Logger

	// budgetFn returns the current cap on total engine token spend per UTC day
	// for this project. Read before each billable call so admin/wizard edits
	// apply live. A nil func or a value <= 0 disables the cap.
	budgetFn func() int
}

// callParams carries everything one call needs. served is the segment-scoped
// "already replayed once" set; nil disables the once-per-segment bookkeeping.
type callParams struct {
	rp            resolvedProvider
	req           llm.CompletionRequest
	policy        StagePolicy
	timeout       time.Duration
	fallback      FallbackHook
	stage         string
	runID         string
	sessionID     string
	iteration     int
	noReplayCache bool
	served        map[string]bool
	// toolBytes is the serialized size of the tool schemas, and toolPrefixChanged
	// marks a call whose tool block differs from the previous call in this segment
	// (a mid-session find_tool load reserialized it). The block rides the cached
	// prefix, so a change re-bills all of it; both are recorded rather than
	// inferred from an input-token spike. Diagnostics only, so they ride
	// callParams instead of the provider request.
	toolBytes         int
	toolPrefixChanged bool
}

// servedProvider is the provider row, transport and model that actually answered
// a call. Distinct from callParams.rp because a fallback swap can answer on a
// different row, and the cost tables key on the row that billed.
type servedProvider struct {
	Name  string
	Type  string
	Model string
}

// call executes one completion. Returns the response, the replay-cache key the
// request hashed to (callers keep it to discard a response they could not use),
// whether the response was served from the replay cache, and an error.
func (c *llmCaller) call(ctx context.Context, p callParams) (*llm.CompletionResponse, string, bool, error) {
	providerType := p.rp.Provider.Type()
	hash := RequestHash(providerType, p.req)
	cacheable := hash != "" && !requestHasImages(p.req)

	if cacheable && !p.noReplayCache && (p.served == nil || !p.served[hash]) {
		if resp := c.cacheRead(hash); resp != nil {
			if p.served != nil {
				p.served[hash] = true
			}
			c.logger.Info("llm call served from replay cache",
				"stage", p.stage, "iteration", p.iteration, "hash", hash[:12])
			c.logCall(p, hash, true, resp, 0, callStats{}, nil, primaryServed(p))
			return resp, hash, true, nil
		}
	}

	// Spend ceiling: a cache hit above is free and never gated, but a billable
	// call is refused once the project has burned its daily engine token budget.
	// Failing here surfaces as a permanent error and pauses the worker.
	// The check also applies the project's own limits (a token budget that overrides
	// the instance default, and a dollar cap), which can bind even when the
	// instance default is unlimited. It fails open on a tally error.
	{
		global := 0
		if c.budgetFn != nil {
			global = c.budgetFn()
		}
		if st := llm.CheckSpend(c.pdb.Reader(), global); st.Exceeded {
			return nil, hash, false, budgetExceeded(st)
		}
	}

	release, err := c.limiter.acquire(ctx, p.rp.ProviderID, p.rp.Model)
	if err != nil {
		return nil, hash, false, err
	}
	defer release()

	// CallLLM returns only the response, so the hook is wrapped to capture the
	// alternate it hands back. Without this the row would name the resolved
	// provider even when a different one billed the call.
	var swapped *servedProvider
	hook := p.fallback
	if hook != nil {
		hook = func(callErr error) (llm.Provider, string) {
			alt, altModel := p.fallback(callErr)
			if alt != nil {
				if altModel == "" {
					altModel = p.rp.Model
				}
				swapped = &servedProvider{Name: alt.Name(), Type: alt.Type(), Model: altModel}
			}
			return alt, altModel
		}
	}

	start := time.Now()
	resp, stats, err := p.policy.CallLLM(ctx, c.logger, p.rp.Provider, p.req, p.timeout, hook)
	duration := time.Since(start).Milliseconds()

	// Attribute to the alternate only when it actually answered. When both failed
	// the error names them both, and the primary is the honest row for the attempt.
	served := primaryServed(p)
	if err == nil && swapped != nil {
		served = *swapped
	}
	c.logCall(p, hash, false, resp, duration, stats, err, served)
	if err != nil {
		return nil, hash, false, err
	}

	// A caller that never reads the replay cache must not write it either:
	// the row would only ever be served to a different caller whose request
	// happens to hash the same (LIVE responses replayed elsewhere).
	if cacheable && !p.noReplayCache {
		c.cacheWrite(hash, p.rp.Model, resp)
	}
	return resp, hash, false, nil
}

// cacheRead returns the cached response for hash, or nil on miss/decode error.
func (c *llmCaller) cacheRead(hash string) *llm.CompletionResponse {
	var raw string
	err := c.pdb.QueryRow(
		"SELECT response_json FROM of_llm_cache WHERE request_hash = ?", hash,
	).Scan(&raw)
	if err != nil {
		return nil
	}
	var resp llm.CompletionResponse
	if derr := json.Unmarshal([]byte(raw), &resp); derr != nil {
		// A corrupt row would otherwise be a permanent, invisible miss that
		// still occupies the cache; log it and purge so the slot can refill.
		c.logger.Warn("replay cache row corrupt, purging", "hash", hash, "error", derr)
		_, _ = sqliteretry.Exec(c.pdb.Writer(),
			"DELETE FROM of_llm_cache WHERE request_hash = ?", hash)
		return nil
	}
	// Hit bookkeeping is best-effort; a failed write must not block the call.
	_, _ = sqliteretry.Exec(c.pdb.Writer(),
		"UPDATE of_llm_cache SET hits = hits + 1, last_hit_at = CURRENT_TIMESTAMP WHERE request_hash = ?", hash)
	return &resp
}

// cacheWrite stores a successful response under its request hash.
func (c *llmCaller) cacheWrite(hash, model string, resp *llm.CompletionResponse) {
	raw, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if _, err := sqliteretry.Exec(c.pdb.Writer(),
		`INSERT INTO of_llm_cache (request_hash, response_json, model)
		 VALUES (?, ?, ?)
		 ON CONFLICT(request_hash) DO UPDATE SET response_json = excluded.response_json`,
		hash, string(raw), model,
	); err != nil {
		c.logger.Debug("replay cache write failed", "error", err)
	}
}

// cacheDiscard drops the cached responses for the given hashes, so an identical later
// request reaches the provider instead of replaying. Only the caller knows whether a
// stored body was usable, so only it can evict one. Best-effort: a failed delete costs a
// replay, not the call.
func (c *llmCaller) cacheDiscard(hashes []string) {
	for _, hash := range hashes {
		if hash == "" {
			continue
		}
		if _, err := sqliteretry.Exec(c.pdb.Writer(),
			"DELETE FROM of_llm_cache WHERE request_hash = ?", hash,
		); err != nil {
			c.logger.Debug("replay cache discard failed", "hash", hash, "error", err)
		}
	}
}

// logCall records the call in of_llm_calls. Cache hits log zero token usage
// (nothing was billed) while keeping the response visible in the timeline.
// primaryServed is the resolved provider, the row that answers unless a
// fallback swap takes over.
func primaryServed(p callParams) servedProvider {
	return servedProvider{Name: p.rp.Provider.Name(), Type: p.rp.Provider.Type(), Model: p.rp.Model}
}

// logCall writes one of_llm_calls row. served names the provider row that
// actually answered: every per-model cost lookup (run_log, budget, the admin
// cost views) keys on providers.name, so a row without it silently prices the
// call from the crude transport estimate instead of the vendor's own ratios.
func (c *llmCaller) logCall(p callParams, hash string, cacheHit bool, resp *llm.CompletionResponse, durationMs int64, stats callStats, callErr error, served servedProvider) {
	if c.dbLog == nil {
		return
	}
	entry := llm.LLMLogEntry{
		Source:            "engine",
		SessionID:         p.sessionID,
		RunID:             p.runID,
		RequestHash:       hash,
		CacheHit:          cacheHit,
		Stage:             p.stage,
		Iteration:         p.iteration,
		Model:             served.Model,
		ProviderType:      served.Type,
		ProviderName:      served.Name,
		RequestSystem:     p.req.System,
		RequestMaxTokens:  p.req.MaxTokens,
		StablePrefixBytes: p.req.StablePrefixBytes,
		StablePrefixMsgs:  p.req.StablePrefixMsgs,
		TotalRequestBytes: p.req.TotalRequestBytes,
		RequestToolBytes:  p.toolBytes,
		ToolPrefixChanged: p.toolPrefixChanged,
		DurationMs:        durationMs,
		Attempts:          stats.Attempts,
		RetryWaitMs:       stats.Waited.Milliseconds(),
		UsedFallback:      stats.UsedFallback,
	}
	if msgs, err := json.Marshal(p.req.Messages); err == nil {
		entry.RequestMessages = string(msgs)
	}
	if len(p.req.Tools) > 0 {
		names := make([]string, 0, len(p.req.Tools))
		for _, t := range p.req.Tools {
			names = append(names, t.Name)
		}
		if tj, err := json.Marshal(names); err == nil {
			entry.RequestTools = string(tj)
		}
	}
	if callErr != nil {
		entry.ErrorMessage = callErr.Error()
	} else if resp != nil {
		entry.ResponseContent = resp.Content
		entry.ResponseStopReason = resp.StopReason
		if len(resp.ToolCalls) > 0 {
			if tcJSON, err := json.Marshal(resp.ToolCalls); err == nil {
				entry.ResponseToolCalls = string(tcJSON)
			}
		}
		if !cacheHit {
			entry.InputTokens = resp.Usage.InputTokens
			entry.OutputTokens = resp.Usage.OutputTokens
			entry.CacheReadInputTokens = resp.Usage.CacheReadInputTokens
			entry.CacheCreationInputTokens = resp.Usage.CacheCreationInputTokens
		}
	}
	c.dbLog.LogLLMCall(entry)
}

// pruneByCutoff deletes rows from table whose cutoffExpr timestamp predates the
// retention window. Both LLM prune paths share this so the window and timestamp
// format live in one place. cutoffExpr and table are trusted constant callers,
// never request data.
func pruneByCutoff(pdb *db.ProjectDB, table, cutoffExpr string, retention time.Duration) (int64, error) {
	cutoff := time.Now().Add(-retention).UTC().Format("2006-01-02 15:04:05")
	res, err := sqliteretry.Exec(pdb.Writer(),
		"DELETE FROM "+table+" WHERE "+cutoffExpr+" < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// pruneReplayCache and pruneLLMCalls delete rows past the retention window.
// Called by Engine.StartMaintenance.
func pruneReplayCache(pdb *db.ProjectDB, retention time.Duration) (int64, error) {
	return pruneByCutoff(pdb, "of_llm_cache", "COALESCE(last_hit_at, created_at)", retention)
}

func pruneLLMCalls(pdb *db.ProjectDB, retention time.Duration) (int64, error) {
	return pruneByCutoff(pdb, "of_llm_calls", "created_at", retention)
}
