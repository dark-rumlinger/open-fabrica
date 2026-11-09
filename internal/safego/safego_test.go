package safego

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestGoRecoversPanic proves a panic in a Go-launched goroutine is contained:
// if it were not, the panic would propagate and crash the test binary.
func TestGoRecoversPanic(t *testing.T) {
	done := make(chan struct{})
	Go("test-task", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not run")
	}
}

// TestSuperviseRestartsThenStops verifies a supervised loop is restarted after a
// panic and stops once the context is cancelled.
func TestSuperviseRestartsThenStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	Supervise(ctx, "test-loop", func() {
		switch calls.Add(1) {
		case 1:
			panic("boom") // first run panics -> supervisor must restart it
		default:
			cancel() // second run cancels so the supervisor stops
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("supervisor did not restart after panic: calls=%d", calls.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// After cancellation the loop must not keep restarting.
	time.Sleep(superviseBackoff + 500*time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("supervisor kept running after cancel: calls=%d, want 2", n)
	}
}
