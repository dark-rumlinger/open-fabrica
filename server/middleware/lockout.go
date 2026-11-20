package middleware

import (
	"container/list"
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
	lru       *list.List // front = most recently seen, back = oldest
	threshold int
	baseDelay time.Duration
	maxDelay  time.Duration
	lastSweep time.Time
}

type lockoutEntry struct {
	key         string
	failures    int
	lockedUntil time.Time
	lastSeen    time.Time
	elem        *list.Element
}

// NewAccountLockout creates a lockout tracker. threshold is the number of
// consecutive failures tolerated before the first lock; baseDelay is the first
// lock duration and maxDelay caps the exponential backoff.
func NewAccountLockout(threshold int, baseDelay, maxDelay time.Duration) *AccountLockout {
	return &AccountLockout{
		entries:   make(map[string]*lockoutEntry),
		lru:       list.New(),
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
		// Bound memory without evicting actively locked accounts: an attacker
		// flooding 50k bogus usernames must not clear a locked victim's record.
		if len(l.entries) >= maxBuckets {
			l.evictOldestUnlockedLocked(now)
		}
		e = &lockoutEntry{key: key}
		e.elem = l.lru.PushFront(e)
		l.entries[key] = e
	} else {
		l.lru.MoveToFront(e.elem)
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
	if e, ok := l.entries[key]; ok {
		l.lru.Remove(e.elem)
		delete(l.entries, key)
	}
}

// sweepLocked drops entries that are no longer locked and have been idle past
// the retention window, walking from the LRU tail. Caller must hold l.mu.
func (l *AccountLockout) sweepLocked(now time.Time) {
	for el := l.lru.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*lockoutEntry)
		if now.Sub(e.lastSeen) <= 15*time.Minute {
			break
		}
		if now.After(e.lockedUntil) {
			l.lru.Remove(el)
			delete(l.entries, e.key)
		}
		el = prev
	}
	l.lastSweep = now
}

// evictOldestUnlockedLocked removes the oldest entry that is NOT currently
// locked (scanning up to 64 tail entries to keep eviction O(1)) so a flood of
// bogus keys cannot reset an active brute-force lockout. If the map still grows
// past 2*maxBuckets (all-locked flood), the LRU tail is evicted as a hard cap.
// Caller must hold l.mu.
func (l *AccountLockout) evictOldestUnlockedLocked(now time.Time) {
	const maxScan = 64
	scanned := 0
	for el := l.lru.Back(); el != nil && scanned < maxScan; el = el.Prev() {
		scanned++
		e := el.Value.(*lockoutEntry)
		if !now.Before(e.lockedUntil) {
			l.lru.Remove(el)
			delete(l.entries, e.key)
			return
		}
	}
	if len(l.entries) >= 2*maxBuckets {
		if el := l.lru.Back(); el != nil {
			e := el.Value.(*lockoutEntry)
			l.lru.Remove(el)
			delete(l.entries, e.key)
		}
	}
}
