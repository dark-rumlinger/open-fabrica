package engine

import (
	"context"
	"log/slog"

	"openfabrica/internal/actionexec"
	"openfabrica/providers/llm"
)

// CallLLM runs one completion for a caller outside a build session (trigger, job
// and server-function completions) under the same retry policy the engine's LIVE
// stage uses, so a transient failure, a 429, or an overloaded provider is ridden
// out instead of failing the action outright. When providerID is known
// (> 0) the call also holds a slot in the engine's shared per-(provider, model)
// limiter, so it counts against the same rate-limit bucket as the builds.
//
// No replay cache and no budget gate: those belong to the engine's own sessions
// (the unattended callers apply the daily budget themselves). It satisfies
// llm.CallFunc.
func (e *Engine) CallLLM(ctx context.Context, providerID int, p llm.Provider, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return callUnderLivePolicy(ctx, e.limiter, e.logger, providerID, p, req)
}

// callUnderLivePolicy is CallLLM's body, shared with a worker's own one-shot
// completions (a scheduled server function's ctx.llm).
func callUnderLivePolicy(ctx context.Context, limiter *llmLimiter, logger *slog.Logger, providerID int, p llm.Provider, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if providerID > 0 && limiter != nil {
		release, err := limiter.acquire(ctx, providerID, req.Model)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	resp, _, err := policyLive.CallLLM(ctx, logger, p, req, 0, nil)
	return resp, err
}

// completeDeps is what a scheduled server function's ctx.llm needs
// (actionexec.Complete), under the same policy as Engine.CallLLM.
func (w *Worker) completeDeps() actionexec.CompleteDeps {
	d := actionexec.CompleteDeps{
		Encryptor:  w.deps.Encryptor,
		HTTPClient: w.deps.LLMHTTPClient,
		BudgetFn:   w.deps.DailyTokenBudgetFn,
		Call: func(ctx context.Context, providerID int, p llm.Provider, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
			return callUnderLivePolicy(ctx, w.limiter, w.logger, providerID, p, req)
		},
	}
	if w.deps.DB != nil {
		d.GlobalDB = w.deps.DB.DB
	}
	return d
}
