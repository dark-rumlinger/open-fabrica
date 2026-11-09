// Package replaybudget caps how often the model may replay a project's user flows
// (of_test.js). A replay is a full browser run, and on an app with deep bugs "replay
// until green" otherwise turns into an open-ended debugging loop inside one build
// session. Past the cap the model stops and the post-build validation (not counted
// here) reports any flow still red to the owner.
package replaybudget

import (
	"sync"
	"time"

	"openfabrica/internal/dbh"
)

const (
	// Max replays per project inside Window. The stall cap (MaxStalls) is the real
	// convergence governor: it ends a loop that stops getting closer to green. This
	// is the backstop for the other shape, where the model keeps earning replays by
	// nudging a failure one step deeper (which resets the stall count) without ever
	// turning a flow green. Kept low so that chase cannot run long: a build that has
	// not converged in this many replays ships its reds to the owner via the
	// post-build sweep instead of burning the session on them.
	Max    = 8
	Window = time.Hour
)

var flows = &Sliding{max: Max, window: Window, used: map[int][]time.Time{}}

// Scope says which budget a replay draws on. A non-empty RunID with a DB uses the
// durable per-run budget (of_flow_budget), so the cap bounds a whole run across the
// build->repair process restart and crash-resume. Otherwise (LIVE owner chat,
// scheduled tasks, tests without a run) it falls back to the in-memory per-project
// budget, whose wall-clock window suits short owner-driven replays.
type Scope struct {
	DB        dbh.Handle
	ProjectID int
	RunID     string
}

func (s Scope) durable() bool { return s.RunID != "" && s.DB != nil }

// Take records one flow replay and reports whether it was within budget.
func Take(s Scope) bool {
	if s.durable() {
		return takeDurable(s.DB, s.RunID)
	}
	return flows.Take(s.ProjectID, time.Now())
}

// Left is how many replays the scope may still take.
func Left(s Scope) int {
	if s.durable() {
		return leftDurable(s.DB, s.RunID)
	}
	return flows.Left(s.ProjectID, time.Now())
}

// Sliding allows max uses per key within a trailing window.
type Sliding struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	used   map[int][]time.Time
}

// NewSliding returns a budget of max uses per key within window.
func NewSliding(max int, window time.Duration) *Sliding {
	return &Sliding{max: max, window: window, used: map[int][]time.Time{}}
}

// live drops key's uses that aged out of the window. Callers hold mu.
func (b *Sliding) live(key int, now time.Time) []time.Time {
	kept := b.used[key][:0]
	for _, t := range b.used[key] {
		if now.Sub(t) < b.window {
			kept = append(kept, t)
		}
	}
	return kept
}

// Take records a use for key at now and reports whether it was within budget.
func (b *Sliding) Take(key int, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.live(key, now)
	if len(kept) >= b.max {
		b.used[key] = kept
		return false
	}
	b.used[key] = append(kept, now)
	return true
}

// Left is how many uses key has left at now.
func (b *Sliding) Left(key int, now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, b.max-len(b.live(key, now)))
}
