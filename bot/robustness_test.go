package bot

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Idle rate-limit buckets are swept once the map reaches its cap, so a stream of
// distinct senders cannot grow it without bound; a recently active bucket stays.
func TestAllowRateSweepsIdleBuckets(t *testing.T) {
	svc, _, _ := newTestService(t)
	old := time.Now().Add(-10 * time.Minute)
	for i := 0; i < rlMaxBuckets; i++ {
		svc.rl.buckets[fmt.Sprintf("old-%d", i)] = &rateBucket{tokens: rlBurst, last: old}
	}
	svc.rl.buckets["recent"] = &rateBucket{tokens: 1, last: time.Now()}

	if !svc.allowRate("newcomer") {
		t.Fatal("a first message was refused")
	}
	if n := len(svc.rl.buckets); n > 3 {
		t.Errorf("%d buckets after the sweep, want only the live ones", n)
	}
	if _, ok := svc.rl.buckets["recent"]; !ok {
		t.Error("an active bucket was swept")
	}
}

// Concurrent allowlist edits must not lose one another: each AddUser reads the
// list, appends and writes it back, so without serialization the last writer wins.
func TestAddUserConcurrentEditsAreAllKept(t *testing.T) {
	svc, _, _ := newTestService(t)
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := svc.AddUser(fmt.Sprintf("user-%d", i), ""); err != nil {
				t.Errorf("AddUser: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(svc.Users()); got != n {
		t.Errorf("allowlist holds %d users after %d concurrent adds", got, n)
	}
}
