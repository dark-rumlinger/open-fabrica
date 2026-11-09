package api

import "testing"

// TestAgentPathEndsInRun guards the naming check that prevents an agent from
// being created at a path that doubles its /run route (e.g. "support/run" would
// serve at /api/support/run/run).
func TestAgentPathEndsInRun(t *testing.T) {
	bad := []string{"run", "support/run", "a/b/run", "RUN", "support/Run"}
	for _, p := range bad {
		if !agentPathEndsInRun(p) {
			t.Errorf("agentPathEndsInRun(%q) = false, want true", p)
		}
	}
	good := []string{"support", "assistant", "orders/copilot", "workflow-run", "runner", "run-desk"}
	for _, p := range good {
		if agentPathEndsInRun(p) {
			t.Errorf("agentPathEndsInRun(%q) = true, want false", p)
		}
	}
}
