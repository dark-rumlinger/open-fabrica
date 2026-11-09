package probe

// Runtime validation: load every page in a headless Chrome and surface uncaught JS
// errors, console.error, and failed asset fetches that static analysis misses. A
// same-origin functional resource that fails to load is a hard error; a cross-origin one
// (declared CDN/font) is only a warning. Degrades gracefully: no Chrome means no issues,
// not a failed build.

import (
	"context"
	"database/sql"
	"fmt"
	neturl "net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"openfabrica/store"
)

// probeAuthTokenKey carries an optional auth JWT: when present the probe seeds
// localStorage.auth_token before any page script runs, so requireRole-gated pages render
// their authenticated data path instead of bouncing to /login.
type probeAuthTokenKeyT struct{}

var probeAuthTokenKey = probeAuthTokenKeyT{}

// WithAuthToken returns a context that makes the probe authenticate every page
// load with the given JWT. An empty token is a no-op (probe stays anonymous).
func WithAuthToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, probeAuthTokenKey, token)
}

func authTokenFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(probeAuthTokenKey).(string); ok {
		return v
	}
	return ""
}

// runtimeProbeTimeout caps how long chromedp spends on a single page: navigate, wait for
// load, then idle so requestAnimationFrame loops surface their first-frame crashes.
const runtimeProbeTimeout = 20 * time.Second

// probeConcurrency is how many pages are probed at once in the shared Chrome. Each
// page is mostly a fixed idle-observation window rather than CPU, so a few in
// parallel cut the sweep proportionally; kept small so the renderers do not contend
// for the browser's main thread and start timing each other out.
const probeConcurrency = 4

// The envelope tripwire and idle window this probe injects live in store
// (ProbeInstrumentationJS, RuntimeInstrumentationAction, RuntimeIdleWindow) so the
// in-build test_page probe loads pages identically. See tools/store/browser_probe.go.

// RuntimeResult bundles the output of a runtime validation pass. Skipped is non-empty
// when the probe never ran (no base URL, headless Chrome unavailable); callers surface it
// as a warning so a missing browser doesn't masquerade as a clean pass.
type RuntimeResult struct {
	Issues  []ValidationIssue
	Skipped string
}

// RunRuntimeValidation navigates every project page in a headless Chrome and collects
// runtime errors. baseURL is the local server root; hostHeader is the project's
// domain, which Chrome resolves to the local server via --host-resolver-rules. No-op
// if baseURL is empty, chromedp cannot allocate a browser, or the project has no pages.
func RunRuntimeValidation(ctx context.Context, db *sql.DB, baseURL, hostHeader string) RuntimeResult {
	if baseURL == "" {
		return RuntimeResult{Skipped: "no base URL configured"}
	}
	pages := loadPagePaths(db)
	if len(pages) == 0 {
		// Headless projects (no pages), nothing to probe, not a skip.
		return RuntimeResult{}
	}

	cfg := store.BrowserConfig{HostResolverMap: hostHeader}
	allocCtx, cancelAlloc, err := store.LaunchBrowser(ctx, cfg)
	if err != nil {
		// Chrome missing / sandbox blocked / offline, don't fail the build,
		// but surface the skip so the user knows runtime errors weren't checked.
		return RuntimeResult{Skipped: fmt.Sprintf("headless browser unavailable: %v", err)}
	}
	// One shared Chrome for the sweep: each page below is a tab of it (in its own browser
	// context), not a process of its own started inside the page's deadline.
	allocCtx, cancelShared, err := startSharedFrom(allocCtx, cancelAlloc)
	if err != nil {
		return RuntimeResult{Skipped: fmt.Sprintf("headless browser unavailable: %v", err)}
	}
	// Track the current allocator's cancel so we can swap it when a page hangs; the
	// deferred cleanup wraps the latest cancel via pointer, not just the initial value.
	currentCancel := cancelShared
	defer func() { currentCancel() }()

	// Which URL to hit: with a hostHeader use it directly (Chrome resolves it to
	// 127.0.0.1 via --host-resolver-rules); otherwise fall back to the raw baseURL.
	origin := baseURL
	if hostHeader != "" {
		origin = replaceHostInURL(baseURL, hostHeader)
	}

	// Optional probe auth token: when set, each page load is seeded with a real
	// session so requireRole-gated pages run their data path under the probe.
	authToken := authTokenFromContext(ctx)

	var issues []ValidationIssue
	// Pages are probed in concurrent batches over the shared Chrome: each already gets
	// its own tab, and the per-page cost is dominated by a fixed idle-observation
	// window, so a sweep of N pages was N times that window for no reason. Batching
	// rather than one flat pool keeps the recycle-on-error rule below meaningful: a
	// batch drains before the decision, so no probe is ever running against a browser
	// that is about to be torn down.
	for start := 0; start < len(pages); start += probeConcurrency {
		end := start + probeConcurrency
		if end > len(pages) {
			end = len(pages)
		}
		batch := pages[start:end]

		found := make([][]ValidationIssue, len(batch))
		var wg sync.WaitGroup
		for i, p := range batch {
			// Substitute :param / {param} / [param] segments before hitting the URL (a raw
			// `/post/:id` the router can't match returns 404). Issue keys keep the template
			// path so reports stay readable. Resolved on this goroutine: it reads the DB,
			// and the handle is not safe to share across the batch.
			concrete := concreteProbePath(db, p)
			wg.Add(1)
			go func(i int, p, url string) {
				defer wg.Done()
				defer func() {
					// A panic in one page's probe must not take down the sweep.
					if r := recover(); r != nil {
						found[i] = nil
					}
				}()
				found[i] = probeRuntime(allocCtx, url, p, authToken)
			}(i, p, origin+concrete)
		}
		wg.Wait()

		batchErrored := false
		for _, pageIssues := range found {
			issues = append(issues, pageIssues...)
			if hasRuntimeErrorIssue(pageIssues) {
				batchErrored = true
			}
		}

		// Recycle the Chrome process when a page in this batch hung: one broken page
		// (infinite loop, fetch storm) can saturate the shared main thread and cascade
		// timeouts into every later probe. Recycling pays a ~1-2s cost once for a clean
		// Chrome. Safe here because the batch has drained.
		if batchErrored && end < len(pages) {
			currentCancel()
			newAlloc, newCancel, relaunchErr := store.LaunchBrowser(ctx, cfg)
			if relaunchErr == nil {
				newAlloc, newCancel, relaunchErr = startSharedFrom(newAlloc, newCancel)
			}
			if relaunchErr != nil {
				// Couldn't relaunch: abandon remaining probes rather than fire spurious
				// failures against a dead allocator. Collected issues still get reported.
				currentCancel = func() {}
				break
			}
			allocCtx = newAlloc
			currentCancel = newCancel
		}

		// Early-abort if the parent ctx is done (stage timeout).
		if ctx.Err() != nil {
			break
		}
	}
	return RuntimeResult{Issues: issues}
}

// startSharedFrom starts one shared browser on a launched allocator and returns it with a
// cancel that closes both. On failure the allocator is cancelled too.
func startSharedFrom(alloc context.Context, cancelAlloc context.CancelFunc) (context.Context, context.CancelFunc, error) {
	shared, cancelShared, err := store.StartSharedBrowser(alloc)
	if err != nil {
		cancelAlloc()
		return nil, nil, err
	}
	return shared, func() { cancelShared(); cancelAlloc() }, nil
}

// hasRuntimeErrorIssue reports whether any issue is a severity=error runtime-probe issue:
// the page hung, fetch-stormed, or threw during load, signalling Chrome is in a bad state.
// Drives the allocator-recycle decision in RunRuntimeValidation.
func hasRuntimeErrorIssue(issues []ValidationIssue) bool {
	for _, iss := range issues {
		if iss.Severity == "error" && iss.Category == "runtime" {
			return true
		}
	}
	return false
}

// concreteProbePath replaces dynamic-route params (:param / {param} / [param]) with
// concrete values the router can resolve: each takes MIN(id) from the dynamic table
// hinted by the preceding segment (singular or plural), falling back to "1".
func concreteProbePath(db *sql.DB, path string) string {
	if !strings.ContainsAny(path, ":{[") {
		return path
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if seg == "" {
			continue
		}
		isParam := false
		switch {
		case strings.HasPrefix(seg, ":"):
			isParam = true
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			isParam = true
		case strings.HasPrefix(seg, "[") && strings.HasSuffix(seg, "]"):
			isParam = true
		}
		if !isParam {
			continue
		}
		// Find the preceding non-empty segment to use as a table-name hint.
		var hint string
		for j := i - 1; j >= 0; j-- {
			if segments[j] != "" {
				hint = segments[j]
				break
			}
		}
		segments[i] = lookupSeededID(db, hint)
	}
	return strings.Join(segments, "/")
}

// lookupSeededID returns MIN(id) from the dynamic table `hint` names (singular or its
// naive plural) so the probe targets a row that actually renders, or "1" by default.
// Errors and missing tables fall back to "1" (best-effort, not exhaustive).
func lookupSeededID(db *sql.DB, hint string) string {
	if db == nil || hint == "" {
		return "1"
	}
	candidates := []string{hint}
	if !strings.HasSuffix(hint, "s") {
		candidates = append(candidates, hint+"s")
	} else {
		candidates = append(candidates, strings.TrimSuffix(hint, "s"))
	}
	for _, name := range candidates {
		// Confirm the dynamic table is registered (same name space; physical
		// name matches the logical name in this codebase).
		var exists int
		if err := db.QueryRow(
			"SELECT 1 FROM of_dynamic_tables WHERE table_name = ? LIMIT 1",
			name,
		).Scan(&exists); err != nil {
			continue
		}
		if !store.ValidTableName.MatchString(name) {
			continue
		}
		var id sql.NullInt64
		if err := db.QueryRow(fmt.Sprintf("SELECT MIN(id) FROM %s", name)).Scan(&id); err == nil && id.Valid {
			return fmt.Sprintf("%d", id.Int64)
		}
	}
	return "1"
}

// hostOf extracts the lowercased host from a URL. Returns "" for unparseable or
// schemeless URLs (data:, blob:, about:), which callers treat as not same-origin.
func hostOf(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// loadFailureKind classifies a network load failure. A same-origin functional resource
// (the project's own script/stylesheet/document/XHR) stays hard ("load_failed");
// everything else is soft ("load_failed_asset", a warning): cosmetic types
// (image/font/media/other) that headless over-reports, and ANY cross-origin failure.
func loadFailureKind(resourceType network.ResourceType, sameOrigin bool) string {
	if !sameOrigin {
		return "load_failed_asset"
	}
	switch resourceType {
	case network.ResourceTypeImage, network.ResourceTypeFont, network.ResourceTypeMedia, network.ResourceTypeOther:
		return "load_failed_asset"
	default:
		return "load_failed"
	}
}

// probeRuntime loads one page and returns any runtime issues found.
func probeRuntime(parent context.Context, url, pagePath, authToken string) []ValidationIssue {
	// A tab of the shared Chrome in its own browser context, so no cookie or storage
	// leaks from one page's probe into the next.
	tabCtx, cancelTab := store.NewIsolatedTab(parent)
	defer cancelTab()
	probeCtx, cancelProbe := context.WithTimeout(tabCtx, runtimeProbeTimeout)
	defer cancelProbe()

	errs := listenPageErrors(tabCtx, url)

	// Enable the runtime + network domains so the listeners fire, then navigate and idle so
	// requestAnimationFrame loops surface first-frame crashes. Pure LOAD, no synthetic
	// interaction: a click pass would be non-deterministic, which defeats the fix loop's
	// persisted-vs-new fingerprinting, so the load captures only what the page does on its own.
	if err := chromedp.Run(probeCtx,
		runtime.Enable(),
		network.Enable(),
		// Register the envelope-field tripwire (and optional seeded auth token) so they run
		// before js/auth.js and every page script. Must precede Navigate to take effect.
		store.RuntimeInstrumentationAction(authToken),
		chromedp.Navigate(url),
		chromedp.Sleep(store.RuntimeIdleWindow),
	); err != nil {
		// Navigation-level failure (DNS, timeout, probe deadline). Enrich the bare chromedp
		// error with signals captured before it fired: HTTP status (so 404 reads as 404, not
		// "context canceled"), the post-encoding URL, and accumulated console errors.
		status, finalURL := errs.mainFrame()
		collected := errs.snapshot()

		// A page that redirects itself while loading (a sign-in page bouncing the signed-in
		// probe user, redirectIfLoggedIn) aborts its own navigation. That is a redirect the
		// page chose, not a load failure; the page it lands on is judged as its own route.
		if strings.Contains(err.Error(), "ERR_ABORTED") && status > 0 && status < 400 {
			return nil
		}

		var parts []string
		parts = append(parts, fmt.Sprintf("navigation error: %v", err))
		if status > 0 && status >= 400 {
			parts = append(parts, fmt.Sprintf("server returned HTTP %d, the stored page path does not match the URL that was requested (a common cause is using `[id]` / `{id}` in the path instead of `:id`, which the router treats literally)", status))
		} else if status > 0 {
			parts = append(parts, fmt.Sprintf("server returned HTTP %d but the page never finished loading, a script may be blocking load (infinite loop, hanging fetch, WebSocket retry storm)", status))
		} else {
			parts = append(parts, "no HTTP response was received, the request may have been dropped or the server returned before a response arrived")
		}
		if finalURL != "" && finalURL != url {
			parts = append(parts, fmt.Sprintf("requested URL: %s (note: may differ from the page path due to percent-encoding)", finalURL))
		}
		collected = filterEnvironmentalNoise(collected)
		if len(collected) > 0 {
			parts = append(parts, "errors captured before the failure:\n  - "+strings.Join(collected, "\n  - "))
		}

		return []ValidationIssue{{
			Severity:      "error",
			Category:      "runtime",
			Page:          pagePath,
			Deterministic: true,
			Message:       fmt.Sprintf("page %q failed to load in headless browser, %s", pagePath, strings.Join(parts, "; ")),
		}}
	}

	// A document answered with an error status (a stored path the router does not match, a
	// 404 or 500 page with a clean body) loads without a single script error, so the
	// error listeners alone would pass it.
	// The custom not-found page answers 404 by design.
	if status, _ := errs.mainFrame(); status >= 400 && pagePath != "/404" {
		return []ValidationIssue{{
			Severity:      "error",
			Category:      "runtime",
			Page:          pagePath,
			Deterministic: true,
			Signature:     fmt.Sprintf("http_%d", status),
			Message: fmt.Sprintf("page %q answered HTTP %d, so visitors get an error page instead of the screen (the stored page path does not match the requested route, or the server failed rendering it: a common cause is `[id]` / `{id}` in the path instead of `:id`).",
				pagePath, status),
		}}
	}

	// Page loaded cleanly; collect what it emitted on its own (nothing collected is a pass).
	// Deliberately does NOT judge what the page PAINTED: a "rendered nothing" verdict is
	// timing-coupled (a cold ESM/WebGL app paints late) and would false-positive a correct
	// page. Blank-page causes are caught by the static import/link validators instead.
	collected := errs.snapshot()

	// Read back any envelope-field tripwires the injected instrumentation recorded (page
	// read `.data`/`.count` off a raw Response). Best-effort: a failed evaluate means none.
	var probeViolations []string
	_ = chromedp.Run(probeCtx, chromedp.Evaluate(`window.__ofProbeViolations || []`, &probeViolations))

	// Drop environmental noise that only fires in the headless probe and vanishes for real
	// users (permission prompts, ResizeObserver spec warnings, service worker on plain http).
	collected = filterEnvironmentalNoise(collected)
	if len(collected) == 0 && len(probeViolations) == 0 {
		return nil
	}

	// Split by kind tag so severity matches impact: [uncaught] and [load_failed] are hard
	// (a failed same-origin asset or a throw during load breaks the page); [console.error]
	// is a warning, promoted to error only when its stack points at a project file ("/files/").
	hardErrs, softErrs := splitHardSoft(collected)

	// Envelope-field tripwires are always hard: the page consumed a fetch Response as if it
	// were parsed JSON, silently emptying lists and flipping data pages into an error state.
	for _, v := range probeViolations {
		hardErrs = append(hardErrs, "[envelope_misuse] "+v)
	}

	var issues []ValidationIssue
	if len(hardErrs) > 0 {
		issues = append(issues, ValidationIssue{
			Severity:      "error",
			Category:      "runtime",
			Page:          pagePath,
			Deterministic: true,
			// Discriminate by the project sites that threw so the fix-loop tells a newly
			// revealed throw from the same one persisting; without it every runtime error on
			// a page collapses to one fingerprint and the second layer of a bug ships unfixed.
			Signature: runtimeSignature(hardErrs),
			Message: fmt.Sprintf(
				"page %q hit runtime errors during load (uncaught exceptions, failed asset loads, or project code that caught and logged a real exception). These break the page for real users:\n  - %s\nFix the offending code or asset URL.",
				pagePath, strings.Join(hardErrs, "\n  - "),
			),
		})
	}
	if len(softErrs) > 0 {
		issues = append(issues, ValidationIssue{
			Severity: "warning",
			Category: "runtime",
			Page:     pagePath,
			Message: fmt.Sprintf(
				"page %q logged console.error during load (informational, fix only if it indicates a real problem):\n  - %s",
				pagePath, strings.Join(softErrs, "\n  - "),
			),
		})
	}

	// Dead-control census, but only on a page that loaded cleanly: a page that threw is
	// already flagged and its listeners may never have attached, so judging its controls
	// dead would be a false positive. Reuses this same loaded tab, no second browser.
	if len(hardErrs) == 0 {
		issues = append(issues, probeDeadControls(probeCtx, pagePath)...)
	}
	return issues
}

// consoleErrorPointsAtProject reports whether a [console.error] body includes a stack
// frame from the project's own JavaScript ("/files/") - the project's code caught and
// logged a real exception, versus harmless library console.error spam with no such frame.
func consoleErrorPointsAtProject(msg string) bool {
	return strings.Contains(msg, "/files/")
}

// runtimeFrame{Named,Anon}Re extract project-code stack frames: `at <fn> (http://host/
// files/<path>:line:col)` (named) or `at http://host/files/<path>:line:col` (anonymous).
// Host, port, `?v=` cache-buster, and line:col are excluded so a frame's key is stable
// across runs and line-shifting edits.
var (
	runtimeFrameNamedRe = regexp.MustCompile(`\bat ([^\s()]+) \(https?://[^/]+/files/([^:)\s?]+)`)
	runtimeFrameAnonRe  = regexp.MustCompile(`\bat https?://[^/]+/files/([^:)\s?]+)`)
)

// runtimeSignature derives a STABLE identity for a page's hard errors from the SET of
// project code sites they name (`fn@path`), sorted and joined: the same unfixed defect
// yields the same signature, while clearing one throw and revealing another changes the
// set. Returns "" when no project frame is present (a bare 404).
func runtimeSignature(hardErrs []string) string {
	keys := map[string]bool{}
	for _, e := range hardErrs {
		for _, m := range runtimeFrameNamedRe.FindAllStringSubmatch(e, -1) {
			keys[m[1]+"@"+m[2]] = true
		}
		for _, m := range runtimeFrameAnonRe.FindAllStringSubmatch(e, -1) {
			keys["@"+m[1]] = true
		}
	}
	if len(keys) == 0 {
		return ""
	}
	list := make([]string, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	sort.Strings(list)
	return strings.Join(list, "|")
}

// environmentalNoisePatterns are case-insensitive substrings that appear only in
// headless-Chrome errors, never in a real user session; matching any drops the error.
// Conservative on purpose: every entry is a permission-gated API or known-harmless warning.
var environmentalNoisePatterns = []string{
	// Permission-gated APIs that throw NotAllowedError in headless without
	// a user gesture or explicit grant.
	"notallowederror",
	"permission denied by user agent",
	"document is not focused",                // clipboard.readText/writeText without focus
	"user didn't interact with the document", // autoplay rejection without gesture
	"play() failed because the user",         // autoplay rejection variant
	"requestfullscreen",                      // fullscreen requires user gesture
	"requestpointerlock",                     // pointer lock requires user gesture
	"wake lock",                              // navigator.wakeLock without permission
	"navigator.mediadevices",                 // getUserMedia without mic/camera grant
	"getusermedia",
	"notification permission",
	"push subscription",

	// Browser warnings that are either spec-mandated noise or localhost-only
	// environment artifacts, not real bugs.
	"resizeobserver loop",                              // spec-ordained chrome warning
	"service worker registration failed",               // no HTTPS on localhost in dev
	"favicon.ico",                                      // auto-synthesized fallback exists
	"the connection used to load resources from ws://", // dev-mode websocket warnings
}

// filterEnvironmentalNoise returns the subset of errs that is NOT headless-only
// environmental noise (case-insensitive substring match against environmentalNoisePatterns).
func filterEnvironmentalNoise(errs []string) []string {
	if len(errs) == 0 {
		return errs
	}
	out := errs[:0:0]
	for _, e := range errs {
		lower := strings.ToLower(e)
		noisy := false
		for _, pat := range environmentalNoisePatterns {
			if strings.Contains(lower, pat) {
				noisy = true
				break
			}
		}
		if !noisy {
			out = append(out, e)
		}
	}
	return out
}

// loadPagePaths returns every published page path in deterministic order, home ('/')
// first so the most-visited route is exercised even if the context is cancelled mid-run.
func loadPagePaths(db *sql.DB) []string {
	rows, err := db.Query(`SELECT path FROM of_files
		WHERE kind = 'page' AND is_deleted = 0 AND status = 'published'
		ORDER BY CASE WHEN path = '/' THEN 0 ELSE 1 END, path`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && p != "" {
			out = append(out, p)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// replaceHostInURL swaps the host in "http://127.0.0.1:5733" with
// "domain.example" preserving port and scheme.
func replaceHostInURL(baseURL, newHost string) string {
	// crude but sufficient: baseURL is always "http://host[:port]"
	i := strings.Index(baseURL, "://")
	if i < 0 {
		return baseURL
	}
	scheme := baseURL[:i+3]
	rest := baseURL[i+3:]
	// replace host, keep port
	port := ""
	if j := strings.Index(rest, ":"); j >= 0 {
		port = rest[j:]
		if k := strings.Index(port, "/"); k >= 0 {
			port = port[:k]
		}
	}
	return scheme + newHost + port
}
