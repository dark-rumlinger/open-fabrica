package db

import (
	"context"
	"strings"
	"testing"
)

// newRuntimeErrorDB returns a migrated project DB and its handle.
func newRuntimeErrorDB(t *testing.T) *ProjectDB {
	t.Helper()
	mgr := NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	return pdb
}

// The whole point of recording: the thrown message never reaches the caller, so
// a reader must be able to get it back for the endpoint that raised it.
func TestRuntimeError_RecordedMessageReadsBack(t *testing.T) {
	h := newRuntimeErrorDB(t).ToolHandle(context.Background())
	thrown := "handler threw: TypeError: could not convert function call parameter 2"

	RecordRuntimeError(h, RuntimeErrorFunction, "vote", thrown)

	re, found := LastRuntimeError(h, RuntimeErrorFunction, "vote")
	if !found {
		t.Fatal("a recorded failure must be readable back")
	}
	if re.Message != thrown {
		t.Errorf("message = %q, want %q", re.Message, thrown)
	}
	if re.CreatedAt == "" {
		t.Error("created_at must be stamped")
	}
}

// A probe reads only its own endpoint's failures: another endpoint's error must
// never be reported as the cause of this one's 500.
func TestRuntimeError_ScopedToPath(t *testing.T) {
	h := newRuntimeErrorDB(t).ToolHandle(context.Background())
	RecordRuntimeError(h, RuntimeErrorFunction, "vote", "vote blew up")

	if _, found := LastRuntimeError(h, RuntimeErrorFunction, "memberships"); found {
		t.Error("an endpoint with no recorded failure must report none")
	}
	if _, found := LastRuntimeError(h, "trigger", "vote"); found {
		t.Error("a different source must not match")
	}
}

// Only the newest failure per endpoint is the live one: a fix that changes the
// error must be what a later read sees.
func TestRuntimeError_NewestWins(t *testing.T) {
	h := newRuntimeErrorDB(t).ToolHandle(context.Background())
	RecordRuntimeError(h, RuntimeErrorFunction, "vote", "first cause")
	RecordRuntimeError(h, RuntimeErrorFunction, "vote", "second cause")

	re, found := LastRuntimeError(h, RuntimeErrorFunction, "vote")
	if !found || re.Message != "second cause" {
		t.Errorf("want the newest failure, got %+v (found=%v)", re, found)
	}
}

// This is a request-path write on an already-failing call, so it stays bounded
// rather than growing with every 500 a broken endpoint serves.
func TestRuntimeError_RingIsBounded(t *testing.T) {
	pdb := newRuntimeErrorDB(t)
	h := pdb.ToolHandle(context.Background())
	for i := 0; i < runtimeErrorRing*3; i++ {
		RecordRuntimeError(h, RuntimeErrorFunction, "vote", "cause")
	}
	var n int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_runtime_errors").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n > runtimeErrorRing {
		t.Errorf("ring holds %d rows, cap is %d", n, runtimeErrorRing)
	}
}

// A thrown exception can carry a whole stack, and the readers quote it into a
// tool result, so one message is clipped.
func TestRuntimeError_MessageClipped(t *testing.T) {
	h := newRuntimeErrorDB(t).ToolHandle(context.Background())
	RecordRuntimeError(h, RuntimeErrorFunction, "vote", strings.Repeat("x", runtimeErrorMaxBytes*2))

	re, found := LastRuntimeError(h, RuntimeErrorFunction, "vote")
	if !found {
		t.Fatal("expected the failure to be recorded")
	}
	if len(re.Message) > runtimeErrorMaxBytes+len("...") {
		t.Errorf("message not clipped: %d bytes", len(re.Message))
	}
}

// Recording runs while a request is already failing, so it must never be the
// thing that turns a 500 into a panic.
func TestRuntimeError_RecordIsBestEffort(t *testing.T) {
	h := newRuntimeErrorDB(t).ToolHandle(context.Background())
	RecordRuntimeError(nil, RuntimeErrorFunction, "vote", "cause")
	RecordRuntimeError(h, RuntimeErrorFunction, "vote", "")

	if _, found := LastRuntimeError(nil, RuntimeErrorFunction, "vote"); found {
		t.Error("a nil handle must report no failure")
	}
	if _, found := LastRuntimeError(h, RuntimeErrorFunction, "vote"); found {
		t.Error("an empty message is not a failure worth recording")
	}
}

// The diagnostics action lists across endpoints, newest first.
func TestRuntimeError_RecentAcrossPaths(t *testing.T) {
	h := newRuntimeErrorDB(t).ToolHandle(context.Background())
	RecordRuntimeError(h, RuntimeErrorFunction, "vote", "vote cause")
	RecordRuntimeError(h, RuntimeErrorFunction, "memberships", "memberships cause")

	got, err := RecentRuntimeErrors(h, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].Path != "memberships" {
		t.Errorf("newest must come first, got %q", got[0].Path)
	}
}
