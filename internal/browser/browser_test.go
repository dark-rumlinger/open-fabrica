package browser

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestProbeTimeoutTracker_PlatformDefaults(t *testing.T) {
	if got := defaultLaunchProbeTimeoutFor("amd64"); got != 5*time.Second {
		t.Fatalf("amd64 baseline = %v, want 5s", got)
	}
	if got := defaultLaunchProbeTimeoutFor("arm64"); got != 15*time.Second {
		t.Fatalf("arm64 baseline = %v, want 15s", got)
	}
	if got := defaultLaunchProbeTimeoutFor("arm"); got != 15*time.Second {
		t.Fatalf("arm baseline = %v, want 15s", got)
	}
}

func TestProbeTimeoutTracker_AdaptsAndDecays(t *testing.T) {
	tr := newProbeTimeoutTracker("amd64")
	d, adaptive := tr.budget(0)
	if !adaptive || d != 5*time.Second {
		t.Fatalf("initial budget = (%v, %v), want (5s, true)", d, adaptive)
	}

	// Slow cold start bumps headroom immediately above observed latency.
	tr.recordSuccess(9 * time.Second)
	d, _ = tr.budget(0)
	if d < 11*time.Second {
		t.Fatalf("expected headroom >= 11s after 9s launch, got %v", d)
	}

	// Repeated fast launches gradually decay back toward baseline without dropping below it.
	for range 20 {
		tr.recordSuccess(400 * time.Millisecond)
	}
	d, _ = tr.budget(0)
	if d < 5*time.Second || d > 7*time.Second {
		t.Fatalf("expected decayed budget near 5s baseline, got %v", d)
	}
}

func TestProbeTimeoutTracker_TimeoutExpandsUpToCap(t *testing.T) {
	tr := newProbeTimeoutTracker("amd64")
	next, retry := tr.recordTimeout(5 * time.Second)
	if !retry || next != 10*time.Second {
		t.Fatalf("first timeout = (%v, %v), want (10s, true)", next, retry)
	}
	next, retry = tr.recordTimeout(20 * time.Second)
	if !retry || next != 30*time.Second {
		t.Fatalf("second timeout = (%v, %v), want (30s, true)", next, retry)
	}
	_, retry = tr.recordTimeout(30 * time.Second)
	if retry {
		t.Fatal("expected no retry once maxLaunchProbeTimeout is reached")
	}
}

func TestLaunchBrowser_RetriesOnceOnColdStartTimeout(t *testing.T) {
	origTracker := defaultProbeTracker
	origRun := probeRun
	t.Cleanup(func() {
		defaultProbeTracker = origTracker
		probeRun = origRun
	})

	defaultProbeTracker = newProbeTimeoutTracker("amd64")
	attempts := 0
	probeRun = func(ctx context.Context) error {
		attempts++
		if attempts == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}

	_, cancel, err := LaunchBrowser(context.Background(), BrowserConfig{})
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	cancel()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if got := CurrentLaunchProbeTimeout(); got <= 5*time.Second {
		t.Fatalf("expected adaptive timeout > 5s baseline after timeout recovery, got %v", got)
	}
}

func TestLaunchBrowser_DoesNotRetryNonTimeoutError(t *testing.T) {
	origTracker := defaultProbeTracker
	origRun := probeRun
	t.Cleanup(func() {
		defaultProbeTracker = origTracker
		probeRun = origRun
	})

	defaultProbeTracker = newProbeTimeoutTracker("amd64")
	attempts := 0
	probeRun = func(ctx context.Context) error {
		attempts++
		return exec.ErrNotFound
	}

	_, _, err := LaunchBrowser(context.Background(), BrowserConfig{})
	if !errors.Is(err, ErrBrowserUnavailable) {
		t.Fatalf("err = %v, want ErrBrowserUnavailable", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (fast fail on missing binary)", attempts)
	}
}

func TestLaunchBrowser_OverrideDisablesRetryAndDrift(t *testing.T) {
	origTracker := defaultProbeTracker
	origRun := probeRun
	t.Cleanup(func() {
		defaultProbeTracker = origTracker
		probeRun = origRun
	})

	defaultProbeTracker = newProbeTimeoutTracker("amd64")
	SetLaunchProbeTimeout(7 * time.Second)

	attempts := 0
	probeRun = func(ctx context.Context) error {
		attempts++
		return context.DeadlineExceeded
	}

	_, _, err := LaunchBrowser(context.Background(), BrowserConfig{})
	if !errors.Is(err, ErrBrowserUnavailable) {
		t.Fatalf("err = %v, want ErrBrowserUnavailable", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 when timeout is pinned", attempts)
	}
	if got := CurrentLaunchProbeTimeout(); got != 7*time.Second {
		t.Fatalf("pinned timeout = %v, want 7s", got)
	}
}
