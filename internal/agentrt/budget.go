package agentrt

import (
	"math"
	"sync"
)

// Budget is a shared, run-level token ceiling. It bounds a whole run rather than
// a single call, which is what makes a multi-step (and, later, multi-agent) loop
// safe to expose: the model can plan and call tools freely up to the pool, then
// the run stops. NewBudget(0) is unlimited. The mutex lets concurrent sub-agents
// draw from one pointer without a data race.
type Budget struct {
	mu    sync.Mutex
	limit int
	used  int
}

// NewBudget returns a budget of limit tokens (input+output). limit <= 0 is
// unlimited.
func NewBudget(limit int) *Budget { return &Budget{limit: limit} }

// Remaining reports the tokens left, or a large sentinel when unlimited or nil,
// so a caller can gate on it uniformly.
func (b *Budget) Remaining() int {
	if b == nil {
		return math.MaxInt32
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return math.MaxInt32
	}
	if r := b.limit - b.used; r > 0 {
		return r
	}
	return 0
}

// Used reports the tokens spent so far (0 for a nil budget).
func (b *Budget) Used() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// add charges tokens against the pool. Nil-safe so an unbudgeted run can pass a
// nil *Budget throughout.
func (b *Budget) add(tokens int) {
	if b == nil || tokens <= 0 {
		return
	}
	b.mu.Lock()
	b.used += tokens
	b.mu.Unlock()
}
