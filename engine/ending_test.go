package engine

import "testing"

// allEndings is every terminator the loop can reach. A new one added without a
// line here fails TestEveryEndingHasAnOutcome.
var allEndings = []ending{
	endModelDone, endChecklistComplete,
	endIterationCap, endReadOnlyStreak, endSegmentsExhausted,
	endToolPause, endApproval,
	endWedged, endContinuationCap, endRepeatStall, endTransportError,
}

// TestEveryEndingHasAnOutcome: every terminator names one of the four outcomes
// and a detail. An ending with an empty outcome would reach a caller as the zero
// value and silently read as neither done nor failed.
func TestEveryEndingHasAnOutcome(t *testing.T) {
	valid := map[Ending]bool{EndDone: true, EndExhausted: true, EndPaused: true, EndFailed: true}
	seen := map[string]bool{}
	for _, e := range allEndings {
		if !valid[e.outcome] {
			t.Errorf("ending %q has outcome %q, which is not one of the four", e.detail, e.outcome)
		}
		if e.detail == "" {
			t.Errorf("ending with outcome %q has no detail: of_sessions could not tell it from any other %s", e.outcome, e.outcome)
		}
		if seen[e.detail] {
			t.Errorf("duplicate detail %q: two mechanisms that share an outcome AND a detail are indistinguishable in a post-mortem", e.detail)
		}
		seen[e.detail] = true
	}
}

// TestExhaustedMeansABudgetRanOut is the consistency check that keeps the model
// honest. EndExhausted must mean a budget was reached, and nothing else: a
// detection threshold (an empty-response streak, a continuation cap) is a
// SYMPTOM, not a budget, and filing one under exhausted would tell a caller to
// grant more of a resource that was never the problem.
func TestExhaustedMeansABudgetRanOut(t *testing.T) {
	budgets := map[string]bool{
		endIterationCap.detail:      true, // spec.MaxIterations
		endReadOnlyStreak.detail:    true, // spec.ReadOnlyForceStop
		endSegmentsExhausted.detail: true, // maxSegments
	}
	for _, e := range allEndings {
		if e.outcome == EndExhausted && !budgets[e.detail] {
			t.Errorf("%q is EndExhausted but names no budget", e.detail)
		}
		if e.outcome != EndExhausted && budgets[e.detail] {
			t.Errorf("%q names a budget but is %q, not EndExhausted", e.detail, e.outcome)
		}
	}
}

// TestPauseOutranksAnOtherStop: a turn that both stalls and asks the owner a
// blocking question must park for the answer. Recording the stall instead would
// drop the question on the floor and report a give-up for a run that is merely
// waiting.
func TestPauseOutranksAnOtherStop(t *testing.T) {
	st := &segState{}
	st.stopWith(endRepeatStall)
	st.stopWith(endToolPause)
	if st.stop.outcome != EndPaused {
		t.Fatalf("pause did not outrank the stall: got %s/%s", st.stop.outcome, st.stop.detail)
	}

	// The reverse is not true: a stall after a pause leaves the pause standing.
	st = &segState{}
	st.stopWith(endToolPause)
	st.stopWith(endRepeatStall)
	if st.stop.outcome != EndPaused {
		t.Fatalf("a later stall displaced the pause: got %s/%s", st.stop.outcome, st.stop.detail)
	}
}

// TestFirstReasonWins: absent a pause, the first mechanism to fire is the one
// that actually happened, so a later one must not overwrite it.
func TestFirstReasonWins(t *testing.T) {
	st := &segState{}
	st.stopWith(endRepeatStall)
	st.stopWith(endIterationCap)
	if st.stop.detail != endRepeatStall.detail {
		t.Fatalf("a later reason overwrote the first: got %q, want %q", st.stop.detail, endRepeatStall.detail)
	}
}
