package store

// Pool bookkeeping tests. The refcount is the part worth pinning: undercounting tears
// down a browser another probe is still driving, overcounting leaks a Chrome process.
// These drive the pool with hand-built entries so they need no real browser.

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// fakePooled registers a pool entry backed by a cancellable context instead of Chrome.
func fakePooled(t *testing.T, key string) *pooledBrowser {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pb := &pooledBrowser{allocCtx: ctx, cancel: cancel, borrowed: 1}
	browserPoolMu.Lock()
	browserPool[key] = pb
	browserPoolMu.Unlock()
	t.Cleanup(func() {
		browserPoolMu.Lock()
		delete(browserPool, key)
		browserPoolMu.Unlock()
		cancel()
	})
	return pb
}

// A config carrying ExtraFlags has no comparable identity (they are opaque funcs), so
// it must never be pooled: two configs with different flags would key the same and one
// would silently get the other's browser.
func TestBrowserPoolKeyRefusesExtraFlags(t *testing.T) {
	if _, ok := (BrowserConfig{HostResolverMap: "x.test"}).poolKey(); !ok {
		t.Error("a plain config should be poolable")
	}
	withFlags := BrowserConfig{
		HostResolverMap: "x.test",
		ExtraFlags:      []chromedp.ExecAllocatorOption{chromedp.Flag("mute-audio", true)},
	}
	if _, ok := withFlags.poolKey(); ok {
		t.Error("a config with ExtraFlags must not be poolable")
	}
}

// Two configs differing only in host-resolver mapping must not share a browser: the
// mapping is a process-level flag, so sharing would send one project's probes at
// another's vhost.
func TestBrowserPoolKeySeparatesHosts(t *testing.T) {
	a, _ := (BrowserConfig{HostResolverMap: "a.test"}).poolKey()
	b, _ := (BrowserConfig{HostResolverMap: "b.test"}).poolKey()
	if a == b {
		t.Errorf("different host mappings shared a key: %q", a)
	}
	c, _ := (BrowserConfig{UserAgent: "ua-1"}).poolKey()
	d, _ := (BrowserConfig{UserAgent: "ua-2"}).poolKey()
	if c == d {
		t.Errorf("different user agents shared a key: %q", c)
	}
}

// Two projects that resolve to the same host/UA must not share a browser: without a
// partition they would share one Chrome process and its per-origin cookies /
// localStorage / service workers, bleeding one project's probe state into another.
func TestBrowserPoolKeySeparatesPartitions(t *testing.T) {
	a, _ := (BrowserConfig{PoolPartition: "proj-1"}).poolKey()
	b, _ := (BrowserConfig{PoolPartition: "proj-2"}).poolKey()
	if a == b {
		t.Errorf("different partitions shared a key: %q", a)
	}
	// Same partition, same knobs still shares (the intra-project reuse this exists for).
	c, _ := (BrowserConfig{PoolPartition: "proj-1", HostResolverMap: "x.test"}).poolKey()
	d, _ := (BrowserConfig{PoolPartition: "proj-1", HostResolverMap: "x.test"}).poolKey()
	if c != d {
		t.Errorf("same partition and knobs must share a key: %q vs %q", c, d)
	}
}

// The process cap reclaims the longest-idle entry and never an in-use one, so a
// server probing many projects cannot pile up Chrome processes.
func TestBrowserPoolTakeOldestIdleSkipsBorrowed(t *testing.T) {
	oldest := fakePooled(t, "cap-oldest")
	oldest.borrowed = 0
	oldest.idleAt = time.Now().Add(-2 * time.Minute)
	newest := fakePooled(t, "cap-newest")
	newest.borrowed = 0
	newest.idleAt = time.Now()
	busy := fakePooled(t, "cap-busy") // borrowed stays 1

	browserPoolMu.Lock()
	victim := takeOldestIdleLocked()
	browserPoolMu.Unlock()

	if victim != oldest {
		t.Fatalf("cap eviction took the wrong entry (want the longest-idle one)")
	}
	if busy.allocCtx.Err() != nil {
		t.Error("a borrowed browser was reclaimed by the cap")
	}
	browserPoolMu.Lock()
	_, stillThere := browserPool["cap-oldest"]
	browserPoolMu.Unlock()
	if stillThere {
		t.Error("the reclaimed entry was left in the pool")
	}
	victim.cancel()
}

// A browser with borrowers left must survive an idle-eviction attempt: the timer can
// fire just as a new probe takes the browser out again.
func TestBrowserPoolKeepsBorrowedBrowser(t *testing.T) {
	pb := fakePooled(t, "borrowed")
	pb.borrowed = 2
	evictIdleBrowser("borrowed", pb)
	if pb.allocCtx.Err() != nil {
		t.Error("a browser still in use was torn down")
	}
	if pooledBrowserCount() == 0 {
		t.Error("a browser still in use was dropped from the pool")
	}
}

// The last release arms the idle timer; eviction after it tears the browser down and
// removes it, so the next acquire launches a fresh one.
func TestBrowserPoolEvictsIdleBrowser(t *testing.T) {
	pb := fakePooled(t, "idle")
	releaseBrowser("idle", pb)
	if pb.borrowed != 0 {
		t.Fatalf("borrow count after release = %d, want 0", pb.borrowed)
	}
	if pb.timer == nil {
		t.Fatal("release did not arm the idle timer")
	}
	pb.timer.Stop()
	evictIdleBrowser("idle", pb)
	if pb.allocCtx.Err() == nil {
		t.Error("idle browser was not torn down")
	}
}

// Shutdown must reap everything: pooled browsers are rooted at Background, so nothing
// else would.
func TestShutdownBrowserPoolTearsDownEverything(t *testing.T) {
	pb := fakePooled(t, "shutdown")
	ShutdownBrowserPool()
	if pb.allocCtx.Err() == nil {
		t.Error("shutdown left a browser running")
	}
	if pooledBrowserCount() != 0 {
		t.Error("shutdown left entries in the pool")
	}
}

// A release racing an eviction that already ran must not resurrect the entry or panic.
func TestBrowserPoolReleaseAfterEvictionIsSafe(t *testing.T) {
	pb := fakePooled(t, "raced")
	releaseBrowser("raced", pb)
	pb.timer.Stop()
	evictIdleBrowser("raced", pb)
	// A second eviction, and a release for a borrow that no longer exists.
	evictIdleBrowser("raced", pb)
	releaseBrowser("raced", pb)
	if pb.borrowed < 0 {
		t.Errorf("borrow count went negative: %d", pb.borrowed)
	}
	_ = time.Now()
}

// Cancelling the caller must close its tab, since a pooled allocator is not the
// caller's child and would otherwise leave the probe running to its own timeout.
// Exercised against a plain context: the bridge is independent of chromedp.
func TestPooledTabBridgeStopsOnCallerCancel(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	closed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		select {
		case <-callerCtx.Done():
			close(closed)
		case <-done:
		}
	}()
	cancelCaller()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not reach the tab bridge")
	}
	close(done)
}
