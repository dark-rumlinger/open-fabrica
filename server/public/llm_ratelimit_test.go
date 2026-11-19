package public

import (
	"testing"
	"time"
)

// Rate-limit keys include the client IP, so a key with no request inside the
// window must be swept, otherwise the map grows with every address that ever called.
func TestEndpointRateLimitSweepsIdleKeys(t *testing.T) {
	rl := newEndpointRateLimiter()
	rl.Lock()
	rl.counts = map[string][]time.Time{
		"idle":   {time.Now().Add(-2 * time.Minute)},
		"recent": {time.Now()},
	}
	rl.lastSweep = time.Time{}
	rl.Unlock()

	h := &Handler{endpointLimiter: rl}
	if !h.checkEndpointRateLimit("caller", 10) {
		t.Fatal("first request from a new key must be admitted")
	}

	rl.Lock()
	defer rl.Unlock()
	if _, ok := rl.counts["idle"]; ok {
		t.Error("idle key survived the sweep")
	}
	if _, ok := rl.counts["recent"]; !ok {
		t.Error("a key with a request inside the window was swept")
	}
	if _, ok := rl.counts["caller"]; !ok {
		t.Error("the calling key was not recorded")
	}
}
