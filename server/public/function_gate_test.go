package public

import (
	"net/http"
	"testing"
)

// The gate is the one answer to "may this call run", for HTTP and agent callers alike.
func TestFunctionGate(t *testing.T) {
	h := &Handler{}
	private := &functionEndpoint{Path: "gate-private", RequiresAuth: true}
	if status, _ := h.functionGate(1, private, false, "anon"); status != http.StatusUnauthorized {
		t.Errorf("anonymous call to an auth-required function: status %d, want 401", status)
	}
	if status, _ := h.functionGate(1, private, true, "u1"); status != 0 {
		t.Errorf("authenticated call refused with %d", status)
	}

	limited := &functionEndpoint{Path: "gate-limited", RateLimit: 1}
	if status, _ := h.functionGate(1, limited, false, "caller"); status != 0 {
		t.Fatalf("first call refused with %d", status)
	}
	if status, _ := h.functionGate(1, limited, false, "caller"); status != http.StatusTooManyRequests {
		t.Errorf("second call inside the window: status %d, want 429", status)
	}
}
