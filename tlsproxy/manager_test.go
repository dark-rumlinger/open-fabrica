package tlsproxy

import "testing"

// The reconcile loop starts at boot whether or not any domain was ready to
// serve, so Stop has to end it on the idle path too - not only on the path where
// listeners were bound. Otherwise a tick lands mid-shutdown and reloads against a
// closing database.
func TestStopEndsReconcileLoopWhenIdle(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.startReconcileLoop()
	m.mu.Unlock()

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() on an idle manager: %v", err)
	}

	m.mu.Lock()
	stopped, loop := m.stopped, m.reconcileStop
	m.mu.Unlock()
	if !stopped {
		t.Error("stopped latch not set, a reconcile already inside Reload could re-bind")
	}
	if loop != nil {
		t.Error("reconcile loop still armed after Stop")
	}
}

// Reload calls Start whenever it finds the manager not running, which is how the
// first domain brings the listeners up. During shutdown that same path would
// re-bind the ports being closed, so the latch has to win. This also proves the
// guard runs before any DB access: the test manager has no database.
func TestStartRefusedAfterStop(t *testing.T) {
	m := newTestManager(t)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start() after Stop should be a no-op, got: %v", err)
	}
	if m.IsRunning() {
		t.Error("Start bound listeners after shutdown")
	}
}

// startReconcileLoop is called from both StartIfDomainsExist and Start, so a
// manager that boots with domains must not end up with two loops ticking.
func TestReconcileLoopIsIdempotent(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.startReconcileLoop()
	first := m.reconcileStop
	m.startReconcileLoop()
	second := m.reconcileStop
	m.mu.Unlock()

	if first != second {
		t.Error("a second startReconcileLoop replaced the channel, leaking the first loop")
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
}
