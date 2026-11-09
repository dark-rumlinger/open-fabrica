package store

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
	"time"

	"github.com/chromedp/chromedp"
)

// ErrBrowserUnavailable is wrapped by LaunchBrowser when Chrome cannot be
// launched (binary missing, sandbox blocked, etc.). Callers use errors.Is to
// decide between "skip" (validate) and "user-facing error" (pdf, scrape).
var ErrBrowserUnavailable = errors.New("headless browser unavailable")

// browserLaunchProbeTimeout bounds the initial chromedp.Run that proves the
// browser actually came up. 5s is long enough for a cold Chrome start on a
// Windows box without making the "Chrome is missing" path drag.
const browserLaunchProbeTimeout = 5 * time.Second

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
	// ExtraFlags are appended after the shared defaults, e.g.
	// chromedp.Flag("disable-blink-features", "AutomationControlled").
	ExtraFlags []chromedp.ExecAllocatorOption
}

// LaunchBrowser builds an exec allocator with project defaults plus cfg, verifies
// Chrome connects within browserLaunchProbeTimeout, and returns the allocator
// context with a single cancel that fully tears the browser down. On failure the
// error wraps ErrBrowserUnavailable so callers can branch on errors.Is.
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

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	startCtx, startCancel := context.WithTimeout(browserCtx, browserLaunchProbeTimeout)
	defer startCancel()
	if err := chromedp.Run(startCtx); err != nil {
		cancelBrowser()
		cancelAlloc()
		return nil, nil, fmt.Errorf("%w: %v", ErrBrowserUnavailable, err)
	}
	cancelBrowser()
	// cancelAlloc tears down both the allocator and any contexts derived
	// from it, so the caller only needs this single cancel.
	return allocCtx, cancelAlloc, nil
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

// findChromeExecutable looks for a Chrome/Chromium binary at well-known
// install paths that chromedp's own discovery misses on Windows. Returns the
// first path that exists, or "" to let chromedp fall back to its default
// search (PATH, common Linux/macOS locations).
func findChromeExecutable() string {
	var candidates []string
	switch goruntime.GOOS {
	case "windows":
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
			os.ExpandEnv(`${LOCALAPPDATA}\Google\Chrome\Application\chrome.exe`),
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	default:
		return ""
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
