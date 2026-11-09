package bot

import (
	"sync"
	"time"
)

// Per-user command rate limit (token bucket): a steady 1/sec with a burst of 12
// keeps a compromised or runaway account from hammering the engine.
const (
	rlRefillPerSec = 1.0
	rlBurst        = 12.0
	// rlMaxBuckets is the size at which idle rate-limit buckets are swept.
	rlMaxBuckets = 1024
)

type rateBucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is the per-user token-bucket store. It owns its own lock, so the
// rate-limit state is isolated from the rest of the bot.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*rateBucket)}
}

// allow reports whether userID may act now, consuming one token. An empty userID
// (unknown sender) is always allowed; the caller gates on isAllowed separately.
func (r *rateLimiter) allow(userID string) bool {
	if userID == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b := r.buckets[userID]
	if b == nil {
		// Every distinct sender (and "deny:" chat key) adds a bucket, so a stream of
		// strangers would otherwise grow the map for the life of the process. A bucket
		// untouched for a minute has refilled completely and carries no information.
		if len(r.buckets) >= rlMaxBuckets {
			for id, old := range r.buckets {
				if now.Sub(old.last) > time.Minute {
					delete(r.buckets, id)
				}
			}
		}
		b = &rateBucket{tokens: rlBurst, last: now}
		r.buckets[userID] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rlRefillPerSec
	if b.tokens > rlBurst {
		b.tokens = rlBurst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
