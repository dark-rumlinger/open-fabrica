package store

// A small pool of live headless-Chrome allocators, keyed on the flags that must
// match. LaunchBrowser stays the way to get a DEDICATED process (the runtime probe
// needs one it can kill outright when a page wedges Chrome); AcquireBrowser is for
// the repeated, short probes that would otherwise pay a cold start each time.
//
// A build's manage_testing probes are the case this exists for: a build can make
// many test_page calls, and launching and tearing down a cold Chrome for each one
// dominates the build's non-LLM time.

import (
	"context"
	"sync"
	"time"
)

// browserIdleTTL is how long a pooled browser stays alive with no borrowers. Long
// enough to span the gaps between probes in one build turn and between turns, short
// enough that an idle server is not sitting on a Chrome process.
const browserIdleTTL = 90 * time.Second

// maxPooledBrowsers caps how many live Chrome processes the pool holds at once. The
// idle TTL bounds a browser's lifetime; this bounds the count so a server probing
// many projects in parallel cannot pile up processes. On overflow the longest-idle
// entry is torn down; when every entry is still borrowed the pool is allowed to
// exceed the cap briefly rather than kill a browser mid-probe.
const maxPooledBrowsers = 8

// pooledBrowser is one live allocator plus the bookkeeping that decides when it may
// be torn down. Rooted at context.Background(), not a borrower's context: the
// process outlives any single probe, so a borrower's cancel must not kill it.
type pooledBrowser struct {
	allocCtx context.Context
	cancel   context.CancelFunc
	borrowed int
	idleAt   time.Time
	timer    *time.Timer
}

var (
	browserPoolMu sync.Mutex
	browserPool   = map[string]*pooledBrowser{}
)

// poolKey identifies a set of allocator flags plus the reuse partition. ExtraFlags
// are opaque funcs with no identity, so a config carrying any is never pooled
// (reported by the ok result). PoolPartition leads the key so two projects that
// resolve to the same host/UA never share a process, and so never share Chrome's
// per-origin storage.
func (c BrowserConfig) poolKey() (string, bool) {
	if len(c.ExtraFlags) > 0 {
		return "", false
	}
	return c.PoolPartition + "\x00" + c.HostResolverMap + "\x00" + c.UserAgent, true
}

// AcquireBrowser returns a live allocator for cfg and a release func the caller MUST
// call when done. The allocator may be shared with other callers, so a caller derives
// its own tab with chromedp.NewContext and never cancels the allocator itself.
//
// Falls back to a dedicated LaunchBrowser (release tears it down) when cfg cannot be
// pooled or the pooled process turns out to be dead, so a caller never has to branch.
func AcquireBrowser(ctx context.Context, cfg BrowserConfig) (context.Context, func(), error) {
	key, poolable := cfg.poolKey()
	if !poolable {
		return launchSharedBrowser(ctx, cfg)
	}

	browserPoolMu.Lock()
	if pb := browserPool[key]; pb != nil {
		if pb.allocCtx.Err() == nil {
			pb.borrowed++
			if pb.timer != nil {
				pb.timer.Stop()
				pb.timer = nil
			}
			browserPoolMu.Unlock()
			return pb.allocCtx, func() { releaseBrowser(key, pb) }, nil
		}
		// Chrome died under us: drop it so the launch below replaces it.
		delete(browserPool, key)
		pb.cancel()
	}
	browserPoolMu.Unlock()

	// Launch outside the lock: a cold start is seconds, and holding the pool mutex
	// across it would serialize every other project's probes behind this one.
	// Rooted at Background so the borrower's ctx cannot tear down a shared process.
	allocCtx, cancel, err := launchSharedBrowser(context.Background(), cfg)
	if err != nil {
		return nil, nil, err
	}

	browserPoolMu.Lock()
	if existing := browserPool[key]; existing != nil && existing.allocCtx.Err() == nil {
		// Another caller raced us to it. Keep theirs, discard ours.
		existing.borrowed++
		if existing.timer != nil {
			existing.timer.Stop()
			existing.timer = nil
		}
		browserPoolMu.Unlock()
		cancel()
		return existing.allocCtx, func() { releaseBrowser(key, existing) }, nil
	}
	// Stay under the process cap: reclaim the longest-idle entry before adding a new
	// one. Cancel it after the lock is dropped, since teardown need not be serialized.
	var evicted *pooledBrowser
	if len(browserPool) >= maxPooledBrowsers {
		evicted = takeOldestIdleLocked()
	}
	pb := &pooledBrowser{allocCtx: allocCtx, cancel: cancel, borrowed: 1}
	browserPool[key] = pb
	browserPoolMu.Unlock()
	if evicted != nil {
		evicted.cancel()
	}
	return allocCtx, func() { releaseBrowser(key, pb) }, nil
}

// launchSharedBrowser launches Chrome and starts one shared browser on it, returning a
// context whose tabs all share the process and one cancel that tears everything down.
func launchSharedBrowser(ctx context.Context, cfg BrowserConfig) (context.Context, context.CancelFunc, error) {
	allocCtx, cancelAlloc, err := LaunchBrowser(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	shared, cancelShared, err := StartSharedBrowser(allocCtx)
	if err != nil {
		cancelAlloc()
		return nil, nil, err
	}
	return shared, func() { cancelShared(); cancelAlloc() }, nil
}

// takeOldestIdleLocked removes and returns the unborrowed pooled browser that has
// been idle longest, or nil when every entry is still in use. It does NOT cancel the
// entry (the caller does, off the lock). The caller holds browserPoolMu.
func takeOldestIdleLocked() *pooledBrowser {
	var victimKey string
	var victim *pooledBrowser
	for k, pb := range browserPool {
		if pb.borrowed > 0 {
			continue
		}
		if victim == nil || pb.idleAt.Before(victim.idleAt) {
			victimKey, victim = k, pb
		}
	}
	if victim == nil {
		return nil
	}
	if victim.timer != nil {
		victim.timer.Stop()
	}
	delete(browserPool, victimKey)
	return victim
}

// releaseBrowser gives one borrow back, arming the idle teardown when the last
// borrower leaves. Idempotent per borrow: a caller that releases twice would
// under-count, so callers defer exactly one release.
func releaseBrowser(key string, pb *pooledBrowser) {
	browserPoolMu.Lock()
	defer browserPoolMu.Unlock()
	pb.borrowed--
	if pb.borrowed > 0 {
		return
	}
	pb.borrowed = 0
	pb.idleAt = time.Now()
	if pb.timer != nil {
		pb.timer.Stop()
	}
	pb.timer = time.AfterFunc(browserIdleTTL, func() { evictIdleBrowser(key, pb) })
}

// evictIdleBrowser tears a browser down once it has been idle for the full TTL.
// Re-checks under the lock because a borrower may have arrived since the timer fired.
func evictIdleBrowser(key string, pb *pooledBrowser) {
	browserPoolMu.Lock()
	if pb.borrowed > 0 || browserPool[key] != pb {
		browserPoolMu.Unlock()
		return
	}
	delete(browserPool, key)
	browserPoolMu.Unlock()
	pb.cancel()
}

// ShutdownBrowserPool tears down every pooled browser. Called on process shutdown:
// these are rooted at Background, so nothing else would reap them.
func ShutdownBrowserPool() {
	browserPoolMu.Lock()
	pending := make([]*pooledBrowser, 0, len(browserPool))
	for k, pb := range browserPool {
		if pb.timer != nil {
			pb.timer.Stop()
		}
		pending = append(pending, pb)
		delete(browserPool, k)
	}
	browserPoolMu.Unlock()
	for _, pb := range pending {
		pb.cancel()
	}
}

// pooledBrowserCount reports how many browsers the pool holds. Test-only visibility
// into state that is otherwise package-private.
func pooledBrowserCount() int {
	browserPoolMu.Lock()
	defer browserPoolMu.Unlock()
	return len(browserPool)
}

// NewPooledTab opens a tab on a pooled allocator and bridges the caller's
// cancellation onto it.
//
// A dedicated allocator is a child of its caller's context, so cancelling the caller
// tears the tab down. A POOLED one is rooted at Background (it outlives any single
// caller), so that link is gone and a stopped caller would otherwise leave its probe
// running until the probe's own timeout. This restores it without giving a caller the
// power to kill a browser others are using.
//
// The returned cancel stops the bridge and closes the tab; it is safe to call twice.
func NewPooledTab(caller, allocCtx context.Context) (context.Context, context.CancelFunc) {
	tabCtx, cancelTab := NewIsolatedTab(allocCtx)
	done := make(chan struct{})
	go func() {
		select {
		case <-caller.Done():
			cancelTab()
		case <-done:
		}
	}()
	var once sync.Once
	return tabCtx, func() {
		once.Do(func() { close(done) })
		cancelTab()
	}
}
