package db

import (
	"testing"
	"time"
)

func TestProjectDBManagerEvictIdleAndDeleteCleanup(t *testing.T) {
	mgr := NewProjectDBManager(t.TempDir())
	defer mgr.CloseAll()

	p1, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("Create(1): %v", err)
	}
	p2, err := mgr.Create(2)
	if err != nil {
		t.Fatalf("Create(2): %v", err)
	}

	// Age project 1 past the idle threshold, keep project 2 fresh.
	p1.lastUsed.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	p2.lastUsed.Store(time.Now().UnixNano())

	if evicted := mgr.EvictIdle(5 * time.Minute); evicted != 1 {
		t.Fatalf("EvictIdle() = %d, want 1", evicted)
	}
	if mgr.Get(1) != nil {
		t.Errorf("project 1 should have been evicted")
	}
	if mgr.Get(2) == nil {
		t.Errorf("project 2 should still be cached")
	}

	// Reopening project 1 should succeed transparently.
	reopened, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("Open(1) after eviction: %v", err)
	}
	if reopened == nil {
		t.Fatalf("Open(1) returned nil")
	}

	// Delete should also prune opLocks[1].
	if err := mgr.Delete(1); err != nil {
		t.Fatalf("Delete(1): %v", err)
	}
	mgr.opMu.Lock()
	_, hasLock := mgr.opLocks[1]
	mgr.opMu.Unlock()
	if hasLock {
		t.Errorf("opLocks[1] was not pruned on Delete")
	}
}
