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
	idle := &endpointRateBucket{key: "idle", windowStart: time.Now().Add(-3 * time.Minute), lastSeen: time.Now().Add(-3 * time.Minute), currCount: 1}
	recent := &endpointRateBucket{key: "recent", windowStart: time.Now(), lastSeen: time.Now(), currCount: 1}
	recent.elem = rl.lru.PushFront(recent)
	idle.elem = rl.lru.PushBack(idle)
	rl.buckets["idle"] = idle
	rl.buckets["recent"] = recent
	rl.lastSweep = time.Time{}
	rl.Unlock()

	h := &Handler{endpointLimiter: rl}
	if !h.checkEndpointRateLimit("caller", 10) {
		t.Fatal("first request from a new key must be admitted")
	}

	rl.Lock()
	defer rl.Unlock()
	if _, ok := rl.buckets["idle"]; ok {
		t.Error("idle key survived the sweep")
	}
	if _, ok := rl.buckets["recent"]; !ok {
		t.Error("a key with a request inside the window was swept")
	}
	if _, ok := rl.buckets["caller"]; !ok {
		t.Error("the calling key was not recorded")
	}
}
