package llm

import "context"

// CallFunc runs one completion under a caller-supplied policy: the engine
// provides one (Engine.CallLLM) that applies its retry budget for transient,
// rate-limit, and timeout failures and, when providerID is known (> 0), holds a
// slot in the shared per-(provider, model) concurrency limiter. Callers outside
// the build loop (trigger, job and server-function completions, all through
// actionexec.Complete) take a CallFunc so they
// get the same resilience without importing the engine.
type CallFunc func(ctx context.Context, providerID int, p Provider, req CompletionRequest) (*CompletionResponse, error)

// Call runs req through f, or straight through p when f is nil (tests, or a
// caller wired without a policy).
func (f CallFunc) Call(ctx context.Context, providerID int, p Provider, req CompletionRequest) (*CompletionResponse, error) {
	if f == nil {
		return p.Complete(ctx, req)
	}
	return f(ctx, providerID, p, req)
}
