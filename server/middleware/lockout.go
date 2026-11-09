package middleware

import (
	"sync"
	"time"
)

// AccountLockout throttles repeated failed authentication against a single
// account, independent of source IP: per-IP rate limiting alone does not stop a
// distributed/IP-rotating guess against one known username. After threshold
// consecutive failures the account is locked for an exponentially increasing
// window (baseDelay, 2x, 4x, ... capped at maxDelay); successful authentication
// clears the record via Reset. State is in-memory on purpose: an attacker
// cannot restart the server, so persistence adds nothing to slowing a guess.
type AccountLockout struct {
	mu        sync.Mutex
	entries   map[string]*lockoutEntry
	threshold int
	baseDelay time.Duration
	maxDelay  time.Duration
	lastSweep time.Time
}

type lockoutEntry struct {
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
}

// NewAccountLockout creates a lockout tracker. threshold is the number of
// consecutive failures tolerated before the first lock; baseDelay is the first
// lock duration and maxDelay caps the exponential backoff.
func NewAccountLockout(threshold int, baseDelay, maxDelay time.Duration) *AccountLockout {
	return &AccountLockout{
		entries:   make(map[string]*lockoutEntry),
		threshold: threshold,
		baseDelay: baseDelay,
		maxDelay:  maxDelay,
		lastSweep: time.Now(),
	}
}

// Locked reports whether key is currently locked and, if so, how long remains.
func (l *AccountLockout) Locked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		return false, 0
	}
	now := time.Now()
	if now.Before(e.lockedUntil) {
		return true, e.lockedUntil.Sub(now)
	}
	return false, 0
}

// Fail records one failed attempt for key. Once the failure count reaches the
// threshold the key is locked for an exponentially growing window.
func (l *AccountLockout) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Sub(l.lastSweep) > time.Minute {
		l.sweepLocked(now)
	}

	e, ok := l.entries[key]
	if !ok {
		// Bound memory the same way the rate limiter does.
		if len(l.entries) >= maxBuckets {
			l.sweepLocked(now)
			if len(l.entries) >= maxBuckets {
				l.evictOldestLocked()
			}
		}
		e = &lockoutEntry{}
		l.entries[key] = e
	}

	e.failures++
	e.lastSeen = now
	if e.failures >= l.threshold {
		// shift = failures past the threshold, so the first lock is baseDelay.
		shift := e.failures - l.threshold
		delay := l.baseDelay
		for i := 0; i < shift && delay < l.maxDelay; i++ {
			delay *= 2
		}
		if delay > l.maxDelay {
			delay = l.maxDelay
		}
		e.lockedUntil = now.Add(delay)
	}
}

// Reset clears the failure record for key. Call on successful authentication.
func (l *AccountLockout) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// sweepLocked drops entries that are no longer locked and have been idle past
// the retention window. Caller must hold l.mu.
func (l *AccountLockout) sweepLocked(now time.Time) {
	for k, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.lastSeen) > 15*time.Minute {
			delete(l.entries, k)
		}
	}
	l.lastSweep = now
}

// evictOldestLocked removes the least-recently-seen entry so a new key can be
// admitted at the cap. Caller must hold l.mu.
func (l *AccountLockout) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, e := range l.entries {
		if first || e.lastSeen.Before(oldest) {
			oldestKey, oldest, first = k, e.lastSeen, false
		}
	}
	if oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}
