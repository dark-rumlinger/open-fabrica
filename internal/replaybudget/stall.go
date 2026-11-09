package replaybudget

import (
	"sync"
	"time"
)

const (
	// MaxStalls is how many replays in a row may fail no fewer flows than the best so far
	// before more are refused. The first red only sets the baseline. The user_flows guide,
	// the live frame and the planned checklist item all tell the model to stop after this
	// many, so it is the one number behind those sentences.
	MaxStalls = 3
	// stallExpiry forgets stale state so it never blocks a later build.
	stallExpiry = 15 * time.Minute
)

var stalls = &Stalls{state: map[int]*stallState{}}

// Observe records a replay (failed flows, 0 for green; how far the failing flows got;
// full suite or not) and returns the replays in a row without progress.
func Observe(s Scope, failed, depth int, full bool) int {
	if s.durable() {
		return observeDurable(s.DB, s.RunID, failed, depth, full)
	}
	return stalls.Observe(s.ProjectID, failed, depth, full, time.Now())
}

// Stalled reports whether the scope is at the cap.
func Stalled(s Scope) bool {
	if s.durable() {
		return stalledDurable(s.DB, s.RunID)
	}
	return stalls.Count(s.ProjectID, time.Now()) >= MaxStalls
}

// LastRed reports whether the scope's latest replay failed.
func LastRed(s Scope) bool {
	if s.durable() {
		return lastRedDurable(s.DB, s.RunID)
	}
	return stalls.LastRed(s.ProjectID, time.Now())
}

// ResetStalls forgets the scope's stall state (never the replays already taken).
func ResetStalls(s Scope) {
	if s.durable() {
		resetStallsDurable(s.DB, s.RunID)
		return
	}
	stalls.Reset(s.ProjectID)
}

type stallState struct {
	best      int // fewest failed flows seen in a red full run; -1 before the first
	bestDepth int // furthest the failing flows got at that count (sum of failing step numbers)
	// partialDepth is the furthest a red single-flow replay got since the last full run.
	// One failing flow replayed on its own gets further each time it is fixed a step, which
	// is progress even though the count of failed flows cannot drop below one.
	partialDepth int
	count        int
	red          bool
	lastAt       time.Time
}

// Stalls tracks replays without progress per project.
type Stalls struct {
	mu    sync.Mutex
	state map[int]*stallState
}

// NewStalls returns an empty tracker.
func NewStalls() *Stalls { return &Stalls{state: map[int]*stallState{}} }

// live returns the project's state, or nil once it has expired. Callers hold mu.
func (s *Stalls) live(key int, now time.Time) *stallState {
	st := s.state[key]
	if st != nil && now.Sub(st.lastAt) >= stallExpiry {
		delete(s.state, key)
		return nil
	}
	return st
}

// Observe records a replay. A green full run clears the state. A red full run with fewer
// failed flows than the best, or the same number failing further into their steps, is
// progress; any other red adds a stall. A green partial run resets the count.
func (s *Stalls) Observe(key, failed, depth int, full bool, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.live(key, now)
	if st == nil {
		st = &stallState{best: -1}
		s.state[key] = st
	}
	st.lastAt = now
	switch {
	case failed == 0 && full:
		delete(s.state, key)
		return 0
	case failed == 0:
		st.count, st.red = 0, false
	case !full && depth > st.partialDepth:
		st.partialDepth, st.count, st.red = depth, 0, true
	case !full:
		st.count++
		st.red = true
	case st.best < 0:
		st.best, st.bestDepth, st.red, st.partialDepth = failed, depth, true, 0
	case failed < st.best || (failed == st.best && depth > st.bestDepth):
		st.best, st.bestDepth, st.count, st.red, st.partialDepth = failed, depth, 0, true, 0
	default:
		st.count++
		st.red = true
	}
	return st.count
}

// Count is the stall count, 0 when the project has none or its state expired.
func (s *Stalls) Count(key int, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.live(key, now); st != nil {
		return st.count
	}
	return 0
}

// LastRed reports whether the latest recorded replay failed.
func (s *Stalls) LastRed(key int, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.live(key, now)
	return st != nil && st.red
}

// Reset drops the project's state.
func (s *Stalls) Reset(key int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state, key)
}
