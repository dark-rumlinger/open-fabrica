package tools

import "testing"

// The dedup is eviction-aware: a delivered, unchanged body is stubbed on EVERY
// repeat read for as long as it is resident, and re-served in full exactly once
// after Forget marks it evicted. No flip-flop while resident.
func TestReadTracker_EvictionAwareStub(t *testing.T) {
	rt := NewReadTracker()
	k, fp := ReadCacheKey("file", "js/state.js"), ReadFingerprint("const a = 1;")

	// Never delivered -> serve (no stub).
	if rt.ShouldStubRange(k, fp, 1, 1) {
		t.Fatal("never-delivered body must not be stubbed")
	}
	rt.RecordDelivered(k, fp)

	// Resident -> stub on every repeat, no re-serve flip-flop.
	for i := 0; i < 5; i++ {
		if !rt.ShouldStubRange(k, fp, 1, 1) {
			t.Fatalf("repeat %d of a resident unchanged body should stub", i)
		}
	}

	// Eviction -> next read serves in full.
	rt.Forget(k)
	if rt.ShouldStubRange(k, fp, 1, 1) {
		t.Fatal("an evicted body must serve in full once, not stub")
	}

	// The re-serve re-records residence, so repeats stub again.
	rt.RecordDelivered(k, fp)
	if !rt.ShouldStubRange(k, fp, 1, 1) {
		t.Fatal("after re-delivery the body is resident again and should stub")
	}
}

func TestReadTracker_ForgetUnknownIsNoOp(t *testing.T) {
	rt := NewReadTracker()
	k, fp := ReadCacheKey("file", "js/a.js"), ReadFingerprint("body-a")
	rt.RecordDelivered(k, fp)
	rt.Forget(ReadCacheKey("file", "js/other.js")) // unknown
	if !rt.ShouldStubRange(k, fp, 1, 1) {
		t.Fatal("forgetting an unrelated key must not evict a resident body")
	}
}

// A changed fingerprint (the artifact was edited) always serves in full.
func TestReadTracker_FingerprintShiftServes(t *testing.T) {
	rt := NewReadTracker()
	k := ReadCacheKey("file", "js/state.js")
	fp1 := ReadFingerprint("old body")
	fp2 := ReadFingerprint("new body") // after an edit
	rt.RecordDelivered(k, fp1)
	if rt.ShouldStubRange(k, fp2, 1, 1) {
		t.Fatal("an edited (fingerprint-shifted) body must serve in full, not stub")
	}
}

// Range dedup: a paginated read of a big file delivers line windows, not the
// whole body, so a repeat read of an already-delivered window is stubbed even
// though the file was never sent in full. A not-yet-delivered window still
// serves, and a full delivery covers any window.
func TestReadTracker_RangeDedup(t *testing.T) {
	rt := NewReadTracker()
	k, fp := ReadCacheKey("file", "js/game.js"), ReadFingerprint("big module body")

	// Nothing delivered -> serve any window.
	if rt.ShouldStubRange(k, fp, 1, 100) {
		t.Fatal("never-delivered window must serve")
	}
	rt.RecordDeliveredRange(k, fp, 1, 100)
	rt.RecordDeliveredRange(k, fp, 100, 200) // adjacent/overlapping -> merges to [1,200]

	// A window inside the delivered span stubs; a window reaching past it serves.
	if !rt.ShouldStubRange(k, fp, 1, 100) {
		t.Fatal("repeat of a delivered window should stub")
	}
	if !rt.ShouldStubRange(k, fp, 50, 180) {
		t.Fatal("window within the merged [1,200] span should stub")
	}
	if rt.ShouldStubRange(k, fp, 150, 300) {
		t.Fatal("window past the delivered span must serve the uncovered tail")
	}

	// A fingerprint shift (an edit) drops all delivered ranges.
	fp2 := ReadFingerprint("big module body v2")
	if rt.ShouldStubRange(k, fp2, 1, 100) {
		t.Fatal("after an edit, a previously delivered window must re-serve")
	}

	// A full delivery covers any window.
	rt.RecordDelivered(k, fp2)
	if !rt.ShouldStubRange(k, fp2, 9000, 9100) {
		t.Fatal("a full delivery should cover any window")
	}
}

// A nil tracker is inert and never stubs (chat/jobs/tests have no tracker).
func TestReadTracker_NilInert(t *testing.T) {
	var rt *ReadTracker
	rt.RecordDelivered("k", "fp")              // must not panic
	rt.RecordDeliveredRange("k", "fp", 1, 100) // must not panic
	if rt.ShouldStubRange("k", "fp", 1, 100) {
		t.Fatal("nil tracker must not stub")
	}
	rt.Forget("k")                     // must not panic
	rt.RecordEditFailure("k")          // must not panic, returns 0
	_ = rt.CumulativeEditFailures("k") // must not panic
	rt.ClearEditFailures("k")          // must not panic
}

// The consecutive edit-failure streak resets on a successful write (ClearEditFailures),
// but the cumulative count does not: a distributed loop (misses interleaved with
// successes, never twice in a row) still accrues so the write path can escalate on it.
func TestReadTracker_EditFailureStreaks(t *testing.T) {
	rt := NewReadTracker()
	k := ReadCacheKey("file", "js/mod.js")

	if got := rt.RecordEditFailure(k); got != 1 {
		t.Fatalf("first failure: consecutive = %d, want 1", got)
	}
	if got := rt.RecordEditFailure(k); got != 2 {
		t.Fatalf("second failure: consecutive = %d, want 2", got)
	}
	if got := rt.CumulativeEditFailures(k); got != 2 {
		t.Fatalf("cumulative = %d, want 2", got)
	}

	// A successful write clears the consecutive streak only.
	rt.ClearEditFailures(k)
	if got := rt.RecordEditFailure(k); got != 1 {
		t.Fatalf("after a clear the consecutive streak restarts at %d, want 1", got)
	}
	if got := rt.CumulativeEditFailures(k); got != 3 {
		t.Fatalf("cumulative must survive the clear: got %d, want 3", got)
	}

	// Another key is independent.
	if got := rt.CumulativeEditFailures(ReadCacheKey("file", "js/other.js")); got != 0 {
		t.Fatalf("an untouched key has no failures, got %d", got)
	}
}
