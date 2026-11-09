package engine

// Guard: a long build must not be left with only an empty task-start baseline
// plus an unbounded chain of deltas.
//
// refreshBaselineDelta emits ONE full baseline per segment, then deltas - sound
// only while the model still holds what the deltas describe. For a fresh build
// the baseline is an EMPTY project, so the chain is the model's ONLY
// consolidated account of what it built; and compaction exists to DEFER the
// rollover that would have re-issued a baseline, so the better compaction
// works, the longer the chain grows while the bodies it assumed were held get
// evicted. Past a certain chain length the model re-reads files it just wrote.
// These tests fail without the forceBaseline path.

import (
	"strings"
	"testing"

	"openfabrica/engine/snapshotbuild"
	"openfabrica/providers/llm"
)

// snapWith builds a snapshot carrying n files, with entity fingerprints that shift as
// n grows so each refresh reads as a real change.
func snapWith(n int) *snapshotbuild.ProjectSnapshot {
	s := &snapshotbuild.ProjectSnapshot{Entities: map[string]string{}}
	for i := 0; i < n; i++ {
		p := "js/mod" + string(rune('a'+i)) + ".js"
		s.Files = append(s.Files, snapshotbuild.FileRef{Path: p, Kind: "js", Scope: "page"})
		s.Entities["file:"+p] = "fp" + string(rune('a'+i))
	}
	return s
}

func snapshotMessages(tr *Transcript) []llm.Message {
	var out []llm.Message
	for _, m := range tr.Messages() {
		if strings.Contains(m.Content, snapshotBaselineSentinel) || strings.Contains(m.Content, snapshotDeltaSentinel) {
			out = append(out, m)
		}
	}
	return out
}

// TestCompactionReissuesBaselineAfterLongDeltaChain: once the chain is long enough to
// be unreplayable AND compaction has started evicting bodies, the next refresh must be
// a full snapshot, not another delta.
func TestCompactionReissuesBaselineAfterLongDeltaChain(t *testing.T) {
	s := &session{}
	st := &sessionSnapshot{mode: SnapshotBaselineDelta}
	tr := &Transcript{}

	// Task-start baseline over an empty project - what a fresh build really gets.
	s.refreshBaselineDelta(st, tr, snapWith(0))
	if st.baselines != 1 {
		t.Fatalf("expected the task-start baseline, got baselines=%d", st.baselines)
	}

	// Build up a delta chain, as a long build does.
	for i := 1; i <= rebaselineAfterDeltas; i++ {
		s.refreshBaselineDelta(st, tr, snapWith(i))
	}
	if st.deltas < rebaselineAfterDeltas {
		t.Fatalf("expected >=%d deltas to accumulate, got %d - the chain-length trigger cannot fire", rebaselineAfterDeltas, st.deltas)
	}
	if st.baselines != 1 {
		t.Fatalf("a delta chain must not re-baseline on its own; baselines=%d", st.baselines)
	}

	// The chain hits the length threshold -> session.go sets forceBaseline.
	st.forceBaseline = true
	s.refreshBaselineDelta(st, tr, snapWith(rebaselineAfterDeltas+1))

	if st.baselines != 2 {
		t.Fatalf("compaction past a %d-delta chain must re-issue a FULL baseline, got baselines=%d.\n"+
			"Without it the model's only account of what it built is an EMPTY task-start snapshot plus "+
			"%d scattered deltas - and re-reading the files is cheaper than replaying them.",
			rebaselineAfterDeltas, st.baselines, st.deltas)
	}
	if st.forceBaseline {
		t.Error("forceBaseline must be cleared once served, or every later refresh re-sends a full snapshot")
	}
	if st.deltas != 0 {
		t.Errorf("the delta chain must reset behind a re-baseline, got deltas=%d", st.deltas)
	}

	last := tr.Messages()[len(tr.Messages())-1]
	if !strings.Contains(last.Content, snapshotBaselineSentinel) {
		t.Fatal("the re-baseline must carry the baseline sentinel, not the delta one")
	}
	// It must supersede, not read as history: the task-start trailer tells the model
	// that earlier project_state messages are historical and this one is the start
	// state - exactly backwards for a mid-build consolidation.
	if strings.Contains(last.Content, baselineTrailer) {
		t.Error("the re-baseline reuses the session-start trailer (\"State at the start of this session\"), " +
			"which frames the CURRENT state as historical. It must supersede the chain above it.")
	}
	if !strings.Contains(last.Content, "supersedes") {
		t.Error("the re-baseline must tell the model it supersedes the chain, or the model still replays the deltas")
	}
}

// TestRebaselineDoesNotFireEveryCompaction: compaction runs on EVERY iteration once
// context sits above the high-water mark, so an ungated re-baseline would append a
// full snapshot per turn - trading a read storm for a write storm.
func TestRebaselineDoesNotFireEveryCompaction(t *testing.T) {
	s := &session{}
	st := &sessionSnapshot{mode: SnapshotBaselineDelta}
	tr := &Transcript{}
	s.refreshBaselineDelta(st, tr, snapWith(0))

	// One change, then a compaction: the chain is still short and replayable.
	s.refreshBaselineDelta(st, tr, snapWith(1))
	if st.deltas >= rebaselineAfterDeltas {
		t.Fatalf("test setup: one delta should be under the threshold, got %d", st.deltas)
	}
	// session.go only sets forceBaseline when deltas >= rebaselineAfterDeltas; mirror
	// that decision here so the gate itself is what is under test.
	if st.deltas >= rebaselineAfterDeltas {
		st.forceBaseline = true
	}
	s.refreshBaselineDelta(st, tr, snapWith(2))

	if st.baselines != 1 {
		t.Errorf("a short, replayable chain must NOT trigger a re-baseline; baselines=%d", st.baselines)
	}
	if n := len(snapshotMessages(tr)); n > 4 {
		t.Errorf("expected a baseline plus a few deltas, got %d snapshot messages - the re-baseline is firing too often", n)
	}
}
