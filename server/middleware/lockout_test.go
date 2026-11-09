package middleware

import (
	"testing"
	"time"
)

func TestAccountLockout_LocksAfterThreshold(t *testing.T) {
	l := NewAccountLockout(3, 50*time.Millisecond, time.Second)

	// Below threshold: not locked.
	l.Fail("alice")
	l.Fail("alice")
	if locked, _ := l.Locked("alice"); locked {
		t.Fatal("locked before reaching threshold")
	}

	// Third failure hits the threshold and locks.
	l.Fail("alice")
	locked, remaining := l.Locked("alice")
	if !locked {
		t.Fatal("expected lock after threshold reached")
	}
	if remaining <= 0 || remaining > 50*time.Millisecond {
		t.Fatalf("unexpected remaining lock %v", remaining)
	}

	// A different account is unaffected.
	if locked, _ := l.Locked("bob"); locked {
		t.Fatal("unrelated account should not be locked")
	}
}

func TestAccountLockout_ExpiresAndResets(t *testing.T) {
	l := NewAccountLockout(1, 30*time.Millisecond, time.Second)

	l.Fail("carol")
	if locked, _ := l.Locked("carol"); !locked {
		t.Fatal("expected immediate lock at threshold 1")
	}

	time.Sleep(45 * time.Millisecond)
	if locked, _ := l.Locked("carol"); locked {
		t.Fatal("lock should have expired")
	}

	// Reset clears the failure record so backoff starts fresh.
	l.Fail("carol")
	l.Reset("carol")
	if locked, _ := l.Locked("carol"); locked {
		t.Fatal("Reset should clear the lock")
	}
}

func TestAccountLockout_BackoffGrows(t *testing.T) {
	l := NewAccountLockout(1, 20*time.Millisecond, 200*time.Millisecond)

	l.Fail("dave") // shift 0 -> 20ms
	_, d1 := l.Locked("dave")
	l.Fail("dave") // shift 1 -> 40ms
	_, d2 := l.Locked("dave")
	if d2 <= d1 {
		t.Fatalf("expected backoff to grow: d1=%v d2=%v", d1, d2)
	}
}
