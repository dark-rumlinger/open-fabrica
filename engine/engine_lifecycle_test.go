package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stopProjectTimeoutForTest shortens the stop deadline and returns a restore.
func stopProjectTimeoutForTest(d time.Duration) func() {
	saved := stopProjectTimeout
	stopProjectTimeout = d
	return func() { stopProjectTimeout = saved }
}

// engineWithStuckWorker registers a worker whose goroutine blocks until release
// is closed, standing in for the real stuck case: a worker inside an LLM call
// that cannot observe its cancelled context before StopProject's deadline.
func engineWithStuckWorker(t *testing.T, projectID int) (*Engine, chan struct{}, *workerHandle) {
	t.Helper()
	e := New(&Deps{}, context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	done := make(chan struct{})
	handle := &workerHandle{worker: &Worker{ctx: ctx, cancel: cancel}, done: done}
	e.workers[projectID] = handle

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(done)
		defer e.forget(projectID, handle)
		<-release
	}()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		e.wg.Wait()
	})
	return e, release, handle
}

// TestStopProjectKeepsTimedOutWorkerReachable is the orphaned-worker regression.
//
// StopProject used to delete the map entry BEFORE waiting, so a worker that
// missed the deadline became unreachable: no later StopProject could find it and
// StopAll could not stop it, while it kept driving a project whose row and
// database project-delete was about to remove. The delete path proceeds on a
// stop error by design, so the map entry is the only remaining handle on it.
func TestStopProjectKeepsTimedOutWorkerReachable(t *testing.T) {
	saved := stopProjectTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(saved)

	e, release, handle := engineWithStuckWorker(t, 7)

	if err := e.StopProject(7); err == nil {
		t.Fatal("a worker that outlasts the deadline must report a timeout")
	}
	if errors.Is(e.StopProject(7), ErrNoWorker) {
		t.Fatal("a timed-out worker must stay registered so a retry can still reach it")
	}

	// Once it finally returns it deregisters itself, and only then is the
	// project genuinely worker-free.
	close(release)
	<-handle.done
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.RLock()
		_, still := e.workers[7]
		e.mu.RUnlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a returned worker must remove its own map entry")
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(e.StopProject(7), ErrNoWorker) {
		t.Fatal("after the worker returned, StopProject must report no worker")
	}
}

// TestForgetKeepsSuccessorRegistered: a project stopped and restarted has a new
// handle, and the OLD goroutine's teardown must not evict its replacement.
func TestForgetKeepsSuccessorRegistered(t *testing.T) {
	e := New(&Deps{}, context.Background())
	old := &workerHandle{done: make(chan struct{})}
	fresh := &workerHandle{done: make(chan struct{})}

	e.workers[3] = fresh
	e.forget(3, old)

	e.mu.RLock()
	got := e.workers[3]
	e.mu.RUnlock()
	if got != fresh {
		t.Fatal("a stale handle's teardown must not deregister the running successor")
	}
}

// TestErrNoWorkerIsDistinguishable pins what project-delete branches on: an idle
// project (no worker) and a stuck worker mean opposite things there, so they
// must not be one indistinguishable error.
func TestErrNoWorkerIsDistinguishable(t *testing.T) {
	e := New(&Deps{}, context.Background())
	if err := e.StopProject(42); !errors.Is(err, ErrNoWorker) {
		t.Fatalf("stopping an unknown project must match ErrNoWorker, got %v", err)
	}
}
