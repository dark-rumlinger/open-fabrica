package browser

// Shared headless-Chrome launcher: every chromedp consumer routes through
// LaunchBrowser for the same defaults, Windows Chrome discovery, and connection
// probe. Callers own their allocators; we deliberately do not share a long-lived
// Chrome process because some flags are process-level and vary per caller.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// ErrBrowserUnavailable is wrapped by LaunchBrowser when Chrome cannot be
// launched (binary missing, sandbox blocked, etc.). Callers use errors.Is to
// decide between "skip" (validate) and "user-facing error" (pdf, scrape).
var ErrBrowserUnavailable = errors.New("headless browser unavailable")

// Adaptive launch-probe bounds: fast desktop hosts start at 5s, slower ARM/SBC
// hosts (e.g. Raspberry Pi) start at 15s, and observed cold-start latency
// scales the budget between 5s and 30s without manual tuning.
const (
	minLaunchProbeTimeout = 5 * time.Second
	maxLaunchProbeTimeout = 30 * time.Second
	armLaunchProbeTimeout = 15 * time.Second
)

func defaultLaunchProbeTimeoutFor(goarch string) time.Duration {
	switch goarch {
	case "arm", "arm64":
		return armLaunchProbeTimeout
	default:
		return minLaunchProbeTimeout
	}
}

type probeTimeoutTracker struct {
	mu       sync.Mutex
	base     time.Duration
	current  time.Duration
	ewma     time.Duration
	override time.Duration
}

func newProbeTimeoutTracker(goarch string) *probeTimeoutTracker {
	base := defaultLaunchProbeTimeoutFor(goarch)
	return &probeTimeoutTracker{
		base:    base,
		current: base,
	}
}

func (t *probeTimeoutTracker) setOverride(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d <= 0 {
		t.override = 0
		return
	}
	t.override = d
}

func (t *probeTimeoutTracker) budget(perCall time.Duration) (time.Duration, bool) {
	if perCall > 0 {
		return perCall, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.override > 0 {
		return t.override, false
	}
	return t.current, true
}

func (t *probeTimeoutTracker) recordSuccess(elapsed time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.override > 0 {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	if t.ewma == 0 {
		t.ewma = elapsed
	} else {
		t.ewma = (3*t.ewma + elapsed) / 4
	}
	headroom := max(t.ewma*2, elapsed+2*time.Second, t.base)
	headroom = min(maxLaunchProbeTimeout, max(minLaunchProbeTimeout, headroom))
	if headroom > t.current {
		t.current = headroom
	} else {
		t.current = max(t.base, (3*t.current+headroom)/4)
	}
}

func (t *probeTimeoutTracker) recordTimeout(attempted time.Duration) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.override > 0 || attempted >= maxLaunchProbeTimeout {
		return attempted, false
	}
	next := min(maxLaunchProbeTimeout, max(attempted*2, minLaunchProbeTimeout))
	if next <= attempted {
		return attempted, false
	}
	t.current = next
	return next, true
}

var (
	defaultProbeTracker = newProbeTimeoutTracker(goruntime.GOARCH)
	probeRun            = func(ctx context.Context) error { return chromedp.Run(ctx) }
)

func init() {
	if raw := strings.TrimSpace(os.Getenv("OPENFABRICA_BROWSER_TIMEOUT")); raw != "" {
		if d, err := parseDurationOrSeconds(raw); err == nil && d > 0 {
			defaultProbeTracker.setOverride(d)
		}
	}
}

func parseDurationOrSeconds(raw string) (time.Duration, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		return d, nil
	}
	var secs int64
	if _, err := fmt.Sscanf(raw, "%d", &secs); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %q", raw)
}

// SetLaunchProbeTimeout pins the headless-browser cold-start probe timeout.
// Passing <= 0 clears the override and restores adaptive scaling.
func SetLaunchProbeTimeout(d time.Duration) {
	defaultProbeTracker.setOverride(d)
}

// CurrentLaunchProbeTimeout returns the active cold-start probe budget.
func CurrentLaunchProbeTimeout() time.Duration {
	d, _ := defaultProbeTracker.budget(0)
	return d
}

// BrowserConfig captures the per-caller knobs on top of the shared defaults.
type BrowserConfig struct {
	// HostResolverMap, when non-empty, adds --host-resolver-rules MAP <host>
	// 127.0.0.1 so Chrome hits the local server for a project's vhost.
	HostResolverMap string
	// UserAgent overrides chromedp's default UA. Empty means "use chromedp's".
	UserAgent string
	// PoolPartition scopes AcquireBrowser reuse to one owner (a project id). A
	// pooled Chrome is only shared within the same partition, so probes of two
	// different projects that resolve to the same 127.0.0.1 origin never share a
	// process, and so never share cookies, localStorage, service workers, or the
	// HTTP cache. Empty means "no partition" (all such callers share one bucket).
	PoolPartition string
	// LaunchTimeout, when > 0, overrides the adaptive cold-start probe timeout
	// for this launch only.
	LaunchTimeout time.Duration
	// ExtraFlags are appended after the shared defaults, e.g.
	// chromedp.Flag("disable-blink-features", "AutomationControlled").
	ExtraFlags []chromedp.ExecAllocatorOption
}

// LaunchBrowser builds an exec allocator with project defaults plus cfg, verifies
// Chrome connects within the adaptive probe timeout, and returns the allocator
// context with a single cancel that fully tears the browser down. On a cold-start
// timeout when adaptive scaling is active, it expands the budget and retries once
// so slower hosts (such as Raspberry Pi / SBCs) recover automatically while missing
// or broken binaries still fail fast. On failure the error wraps
// ErrBrowserUnavailable so callers can branch on errors.Is.
func LaunchBrowser(ctx context.Context, cfg BrowserConfig) (context.Context, context.CancelFunc, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	if cfg.HostResolverMap != "" {
		opts = append(opts, chromedp.Flag("host-resolver-rules",
			fmt.Sprintf("MAP %s 127.0.0.1", cfg.HostResolverMap)))
	}
	if cfg.UserAgent != "" {
		opts = append(opts, chromedp.UserAgent(cfg.UserAgent))
	}
	if execPath := findChromeExecutable(); execPath != "" {
		opts = append(opts, chromedp.ExecPath(execPath))
	}
	opts = append(opts, cfg.ExtraFlags...)

	timeout, adaptive := defaultProbeTracker.budget(cfg.LaunchTimeout)
	allocCtx, cancelAlloc, elapsed, err := launchBrowserAttempt(ctx, opts, timeout)
	if err == nil {
		if adaptive {
			defaultProbeTracker.recordSuccess(elapsed)
		}
		return allocCtx, cancelAlloc, nil
	}

	if adaptive && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		if nextTimeout, retry := defaultProbeTracker.recordTimeout(timeout); retry {
			allocCtx, cancelAlloc, elapsed, err = launchBrowserAttempt(ctx, opts, nextTimeout)
			if err == nil {
				defaultProbeTracker.recordSuccess(elapsed)
				return allocCtx, cancelAlloc, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
}

func launchBrowserAttempt(ctx context.Context, opts []chromedp.ExecAllocatorOption, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration, error) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	startCtx, startCancel := context.WithTimeout(browserCtx, timeout)
	start := time.Now()
	err := probeRun(startCtx)
	elapsed := time.Since(start)
	startCancel()
	if err != nil {
		cancelBrowser()
		cancelAlloc()
		return nil, nil, elapsed, err
	}
	cancelBrowser()
	// cancelAlloc tears down both the allocator and any contexts derived
	// from it, so the caller only needs this single cancel.
	return allocCtx, cancelAlloc, elapsed, nil
}

// StartSharedBrowser starts one Chrome on an allocator context and returns a context
// whose tabs all share that process, with a cancel that closes it.
//
// A context taken straight from an allocator has no browser yet: each chromedp.NewContext
// on it launches its OWN Chrome on first use, inside whatever timeout that first Run
// carries. Starting the browser once here, untimed, and deriving every tab from the
// result (NewIsolatedTab) makes a tab cost a tab, not a process, and keeps a cold start
// out of a probe's deadline. LaunchBrowser already proved Chrome starts, so this does not
// wait on a clock of its own.
func StartSharedBrowser(alloc context.Context) (context.Context, context.CancelFunc, error) {
	shared, cancel := chromedp.NewContext(alloc)
	if err := chromedp.Run(shared); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
	}
	return shared, cancel, nil
}

// NewIsolatedTab opens a tab on a shared browser. Cancel closes the tab.
//
// On a context with no running browser yet (a bare allocator) the tab launches a
// browser of its own on first use, which is the old dedicated-process behavior.
//
// A tab does not get its own browser context: a headless Chrome refuses to open a tab
// in a fresh browser context ("no browser is open"), so tabs of one shared browser share
// cookies and storage. Every caller probes one project's origin and seeds its own
// session state per page, so the sharing is bounded to that project.
func NewIsolatedTab(shared context.Context) (context.Context, context.CancelFunc) {
	return chromedp.NewContext(shared)
}

var (
	chromePathMu   sync.RWMutex
	chromePathConf string
)

// SetExecutablePath overrides headless Chrome/Chromium discovery with an explicit
// binary path (typically from config.json's browser_path). Empty clears the override.
func SetExecutablePath(path string) {
	chromePathMu.Lock()
	chromePathConf = strings.TrimSpace(path)
	chromePathMu.Unlock()
}

func configuredChromePath() string {
	chromePathMu.RLock()
	p := chromePathConf
	chromePathMu.RUnlock()
	return p
}

// findChromeExecutable looks for a Chrome/Chromium binary via explicit config/env
// overrides first, then well-known per-OS install paths. Returns the first path
// that exists, or "" to let chromedp fall back to its default PATH search.
func findChromeExecutable() string {
	candidates := []string{
		configuredChromePath(),
		strings.TrimSpace(os.Getenv("OPENFABRICA_CHROME_PATH")),
		strings.TrimSpace(os.Getenv("CHROME_PATH")),
	}
	switch goruntime.GOOS {
	case "windows":
		candidates = append(candidates,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			os.ExpandEnv(`${LOCALAPPDATA}\Google\Chrome\Application\chrome.exe`),
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		)
	case "darwin":
		candidates = append(candidates,
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		)
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// DetectChromePath performs a cheap, no-launch probe to report whether a
// headless-Chrome binary is reachable, for the boot-time INFO line. Deliberately
// lighter than LaunchBrowser (no Chrome cold start in the boot path); a binary
// present-but-broken still logs as "found" and the next real call surfaces the failure.
func DetectChromePath() string {
	if p := findChromeExecutable(); p != "" {
		return p
	}
	// Names chromedp searches in its default discovery, in roughly the same
	// order. Keeps boot-line truthful: if any of these is on PATH, chromedp
	// will use it.
	for _, name := range []string{
		"google-chrome", "google-chrome-stable", "google-chrome-beta",
		"google-chrome-unstable", "chromium", "chromium-browser",
		"chrome", "microsoft-edge", "microsoft-edge-stable", "headless_shell",
	} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}
