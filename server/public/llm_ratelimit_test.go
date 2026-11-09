package public

import (
	"testing"
	"time"
)

// Rate-limit keys include the client IP, so a key with no request inside the
// window must be swept, otherwise the map grows with every address that ever called.
func TestEndpointRateLimitSweepsIdleKeys(t *testing.T) {
	llmRateLimiter.Lock()
	llmRateLimiter.counts = map[string][]time.Time{
		"idle":   {time.Now().Add(-2 * time.Minute)},
		"recent": {time.Now()},
	}
	llmRateLimiter.lastSweep = time.Time{}
	llmRateLimiter.Unlock()
	t.Cleanup(func() {
		llmRateLimiter.Lock()
		llmRateLimiter.counts = map[string][]time.Time{}
		llmRateLimiter.Unlock()
	})

	if !checkEndpointRateLimit("caller", 10) {
		t.Fatal("first request from a new key must be admitted")
	}

	llmRateLimiter.Lock()
	defer llmRateLimiter.Unlock()
	if _, ok := llmRateLimiter.counts["idle"]; ok {
		t.Error("idle key survived the sweep")
	}
	if _, ok := llmRateLimiter.counts["recent"]; !ok {
		t.Error("a key with a request inside the window was swept")
	}
	if _, ok := llmRateLimiter.counts["caller"]; !ok {
		t.Error("the calling key was not recorded")
	}
}
