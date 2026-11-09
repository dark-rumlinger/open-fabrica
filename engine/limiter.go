package engine

// Per-(provider, model) LLM concurrency limiter. Cloud providers meter rate
// limits PER MODEL, scoped to the account, so the bucket is the (provider,
// model) pair. A slot is held for ONE request, so the cap bounds in-flight
// requests per model; one llmLimiter is shared across workers. The conservative
// default absorbs the caveats (model "groups" share a bucket across versions; a
// local provider is machine-bound) - lower Deps.MaxConcurrentPerModel there.

import (
	"context"
	"sync"
)

// defaultPerModelConcurrency caps concurrent LLM conversations per (provider,
// model) when Deps.MaxConcurrentPerModel is unset. The guard against provider
// rate limits; raise it only as far as the model's RPM/TPM tier allows.
const defaultPerModelConcurrency = 8

// poolKey identifies one rate-limit bucket: a model within a provider account.
type poolKey struct {
	provider int
	model    string
}

// llmLimiter bounds concurrent LLM conversations per (provider, model) pair.
type llmLimiter struct {
	mu      sync.Mutex
	pools   map[poolKey]chan struct{}
	perPool int
}

// newLLMLimiter builds a limiter with the given per-(provider,model) cap
// (clamped to the default when unset).
func newLLMLimiter(perPool int) *llmLimiter {
	if perPool < 1 {
		perPool = defaultPerModelConcurrency
	}
	return &llmLimiter{pools: make(map[poolKey]chan struct{}), perPool: perPool}
}

// pool returns (creating on first use) the semaphore channel for one bucket.
func (l *llmLimiter) pool(providerID int, modelID string) chan struct{} {
	k := poolKey{provider: providerID, model: modelID}
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.pools[k]
	if !ok {
		p = make(chan struct{}, l.perPool)
		l.pools[k] = p
	}
	return p
}

// acquire blocks until a slot for (providerID, modelID) is free or ctx is
// cancelled. On success it returns a release func the caller must defer; on
// cancellation it returns a no-op release and ctx.Err().
func (l *llmLimiter) acquire(ctx context.Context, providerID int, modelID string) (func(), error) {
	p := l.pool(providerID, modelID)
	select {
	case p <- struct{}{}:
		return func() { <-p }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}
