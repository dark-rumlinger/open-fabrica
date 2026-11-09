package observability

// DiagTestModule exercises a saved JS module artifact end-to-end without a page,
// closing the validation gap for headless projects (libraries, game logic) with no
// HTTP surface for test_endpoint/test_page. It navigates Chrome to the page that
// loads the file (its home page when unattached) so page-scope siblings and DOM are
// present and same-origin relative imports resolve, then imports '/files/<path>' and
// calls the entry. The entry resolves as an ES export (named or default) or a window
// global the file installs, so a classic IIFE that sets window.Foo is callable too.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"openfabrica/analysis"
	"openfabrica/internal/stringutil"
	"openfabrica/store"
	"openfabrica/tools"
)

const (
	testModuleDefaultTimeoutMs = 8000
	testModuleMaxTimeoutMs     = 30000
)

// DiagTestModule dynamically imports a saved JS module artifact and calls
// one of its exports. Captures console output and thrown exceptions so a
// failed call surfaces actionable error context, not just a status code.
type DiagTestModule struct {
	FilePath  string `arg:"file_path,required" desc:"Path of the JS module artifact to import (e.g. \"/files/qrcode.js\")."`
	Entry     string `arg:"entry"                  desc:"Function to call: a named ES export, or a window global the file sets (e.g. \"AudioEngine\" for window.AudioEngine, dotted paths like \"App.init\" allowed). Defaults to the module's default export." default:"default"`
	Args      []any  `arg:"args"                   desc:"Positional arguments passed to the entry function." items:"any"`
	TimeoutMs int    `arg:"timeout_ms"             desc:"Per-call execution timeout in milliseconds (maximum 30000)." default:"8000" max:"30000"`
}

func (a *DiagTestModule) ActionName() string { return "test_module" }
func (a *DiagTestModule) Doc() string {
	return "Load a saved JS file in a real browser tab and call one `entry` (an ES export or a window global it installs), capturing console output and exceptions. With no callable entry it passes if the import does not throw. A module that touches document, canvas or events is imported against its owning page without calling its exports: verify those with test_page."
}
func (a *DiagTestModule) ReadOnly() bool { return true }
func (a *DiagTestModule) Example() any {
	return &DiagTestModule{FilePath: "/files/qrcode.js", Args: []any{"hello"}}
}
func (a *DiagTestModule) Run(ctx *tools.ToolContext) (*tools.Result, error) {
	timeoutMs := a.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = testModuleDefaultTimeoutMs
	}
	if timeoutMs > testModuleMaxTimeoutMs {
		timeoutMs = testModuleMaxTimeoutMs
	}

	argsJSONBytes, err := json.Marshal(a.Args)
	if err != nil || a.Args == nil {
		argsJSONBytes = []byte("[]")
	}

	// Collapse a /files/ URL form to the bare path the of_files table keys on, so the
	// existence check and import URL below both address the saved row (a file is stored
	// bare and served at /files/<path>, not the served URL form).
	filePath := store.CanonicalFilePath(a.FilePath)

	// Confirm the artifact exists before paying the chromedp launch cost.
	var n int
	_ = ctx.DB.QueryRow(`SELECT COUNT(*) FROM of_files
		WHERE path = ? AND is_deleted = 0`, filePath).Scan(&n)
	var pathNote string
	if n == 0 {
		// A miss here is nearly always the right file in the wrong directory (flat foo.js
		// referenced as js/foo.js or the reverse). When exactly one stored artifact shares
		// this basename, resolve to it and carry a note; an ambiguous or absent basename
		// still fails with the suggestion hint.
		resolved, ok := store.ResolveUniqueBasename(ctx.DB, "file", filePath)
		if !ok {
			return tools.Fail("artifact %q not found, call manage_files action=list to see what exists.%s", filePath, store.SuggestArtifactPathsHint(ctx.DB, "file", filePath)), nil
		}
		pathNote = fmt.Sprintf("resolved %q to %q (unique basename match: the file exists at a different path than referenced)", filePath, resolved)
		filePath = resolved
	}

	// Grab the served file body (kind='file', what /files/<path> imports) so we can
	// tell whether the module is DOM-coupled (see below).
	var body string
	_ = ctx.DB.QueryRow(`SELECT body FROM of_files
		WHERE path = ? AND kind = 'file' AND is_deleted = 0 LIMIT 1`, filePath).Scan(&body)

	// A DOM-coupled client module (canvas game, input handler, renderer) wires itself
	// to page nodes on a lifecycle, so invoking one of its exports headless and out of
	// order throws a false failure. For these, importing the file (which runs top-level
	// code against the owning page's DOM) is the load check; behaviour is verified at
	// the page level by test_page, not by calling a single export in isolation.
	domCoupled := moduleReferencesDOM(body)

	domain, port, err := testResolveDomain(ctx)
	if err != nil {
		return tools.FailErr(err), nil
	}

	// Outer budget covers Chrome launch (<=5s) + per-call timeout. If launch
	// stalls the whole tool times out cleanly rather than leaking processes.
	parentCtx, cancelParent := context.WithTimeout(ctx.Context(),
		time.Duration(timeoutMs)*time.Millisecond+6*time.Second)
	defer cancelParent()

	// Pooled: a build probes many pages, and a cold Chrome per probe dominates a
	// build's non-LLM time. The tab below is still per-call. Partition
	// the pool by project so a domainless project never shares a process (and its
	// cookies / storage) with another.
	allocCtx, releaseBrowser, err := store.AcquireBrowser(parentCtx, store.BrowserConfig{HostResolverMap: domain, PoolPartition: browserPoolPartition(ctx)})
	if err != nil {
		if errors.Is(err, store.ErrBrowserUnavailable) {
			return tools.Fail("%v, install Chrome/Edge so chromedp can launch", err), nil
		}
		return tools.FailErr(err), nil
	}
	defer releaseBrowser()

	// The pooled allocator is shared, so the tab (not the browser) carries this
	// call's cancellation.
	tabCtx, cancelTab := store.NewPooledTab(parentCtx, allocCtx)
	defer cancelTab()

	var (
		mu         sync.Mutex
		consoleOut []string
		thrownOut  []string
	)
	chromedp.ListenTarget(tabCtx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			parts := []string{string(e.Type)}
			for _, ag := range e.Args {
				if ag == nil {
					continue
				}
				if len(ag.Value) > 0 {
					parts = append(parts, string(ag.Value))
				} else if ag.Description != "" {
					parts = append(parts, ag.Description)
				}
			}
			line := stringutil.Truncate(strings.Join(parts, " "), 600)
			mu.Lock()
			consoleOut = append(consoleOut, line)
			mu.Unlock()
		case *runtime.EventExceptionThrown:
			if e.ExceptionDetails != nil {
				msg := stringutil.Truncate(strings.TrimSpace(e.ExceptionDetails.Error()), 600)
				mu.Lock()
				thrownOut = append(thrownOut, msg)
				mu.Unlock()
			}
		}
	})

	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	if domain != "" {
		origin = fmt.Sprintf("http://%s:%d", domain, port)
	}
	// Run the module in the context of the page that loads it, when attached. A
	// page-scope script commonly depends on a sibling that runs first (a shared global,
	// the page's DOM), so importing it bare on a page lacking those siblings throws a
	// false failure. Navigating to the owning page loads the siblings in attach order
	// first; an unattached file still loads against the home page.
	navPath := "/"
	if page, ok := store.PageForAsset(ctx.DB, filePath); ok {
		navPath = page
	}
	moduleURL := "/files/" + filePath
	entryJSON, _ := json.Marshal(a.Entry)
	urlJSON, _ := json.Marshal(moduleURL)
	domCoupledJSON := "false"
	if domCoupled {
		domCoupledJSON = "true"
	}
	runScript := fmt.Sprintf(`
		(async () => {
			try {
				const m = await import(%s);
				const entryName = %s;
				const domCoupled = %s;
				// Resolve the entry as either an ES export or a (possibly dotted)
				// window global. Importing the file executes it, so an IIFE / classic
				// script that attaches window.Foo has set the global by now - the
				// common non-module pattern for games and JS tools. Try the module
				// namespace first, then its default, then walk entryName on window.
				const onHost = (host, name) => name.split('.').reduce((o, k) => (o == null ? undefined : o[k]), host);
				let fn = m[entryName] !== undefined ? m[entryName] : m.default;
				// A dotted window entry (App.init) is a method: call it on its owner so
				// 'this' is the object, as the page's own code would.
				let thisArg = null;
				if (typeof fn !== 'function') {
					const g = onHost(window, entryName);
					if (g !== undefined && g !== null) {
						fn = g;
						const cut = entryName.lastIndexOf('.');
						if (cut > 0) { thisArg = onHost(window, entryName.slice(0, cut)); }
					}
				}
				if (domCoupled) {
					// DOM-coupled module: do NOT invoke an export in isolation - it would
					// run out of its page lifecycle and throw a false failure. Importing it
					// already ran its top-level code against the owning page's DOM without
					// throwing (or we'd be in catch). Pass as long as the requested export
					// is present; an explicitly named export that is genuinely absent is
					// still a failure.
					const present = entryName === 'default' || (fn !== undefined && fn !== null);
					if (present) {
						return { ok: true, domCoupled: true, exports: Object.keys(m), note: 'DOM-coupled client module: imported and executed cleanly against the page DOM. Its exports were not invoked in isolation (calling one out of its page lifecycle, e.g. start() before init() wires the canvas, throws a false failure). Verify runtime behaviour with test_page.' };
					}
					return { ok: false, error: 'entry ' + entryName + ' not found on DOM-coupled module; checked module exports [' + Object.keys(m).join(',') + '] and window.' + entryName };
				}
				if (typeof fn !== 'function') {
					// No callable entry resolved. When the caller did not request a
					// specific entry (the default), the file is a side-effecting script:
					// importing it ran its top-level code without throwing, which is the
					// pass - there is simply nothing to invoke. Only an explicitly named
					// entry that is absent is a failure (the caller expected an export
					// that is not there).
					if (entryName === 'default') {
						return { ok: true, sideEffecting: true, exports: Object.keys(m), note: 'module imported and executed without throwing; no callable entry to invoke (side-effecting script). For a script that drives the UI, verify behaviour with test_page.' };
					}
					return { ok: false, error: 'entry ' + entryName + ' is not a function (got ' + typeof fn + '); checked module exports [' + Object.keys(m).join(',') + '] and window.' + entryName };
				}
				const callArgs = %s;
				const argList = Array.isArray(callArgs) ? callArgs : [callArgs];
				const result = await fn.apply(thisArg, argList);
				let serialized;
				try { serialized = JSON.parse(JSON.stringify(result)); }
				catch (_) { serialized = String(result); }
				return { ok: true, result: serialized };
			} catch (e) {
				return { ok: false, error: e && e.message ? e.message : String(e), stack: e && e.stack ? String(e.stack) : null };
			}
		})()
	`, urlJSON, entryJSON, domCoupledJSON, string(argsJSONBytes))

	runCtx, cancelRun := context.WithTimeout(tabCtx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancelRun()

	var rawResult json.RawMessage
	err = chromedp.Run(runCtx,
		runtime.Enable(),
		chromedp.Navigate(origin+navPath),
		chromedp.Sleep(150*time.Millisecond),
		chromedp.Evaluate(runScript, &rawResult, withAwaitPromise),
	)

	mu.Lock()
	clines := append([]string(nil), consoleOut...)
	tlines := append([]string(nil), thrownOut...)
	mu.Unlock()

	if err != nil {
		d := map[string]any{
			"file_path": a.FilePath,
			"entry":     a.Entry,
			"console":   clines,
			"thrown":    tlines,
		}
		if pathNote != "" {
			d["path_note"] = pathNote
		}
		return &tools.Result{Success: false, Error: fmt.Sprintf("module execution failed: %v", err), Data: d}, nil
	}

	var parsed struct {
		OK            bool            `json:"ok"`
		Result        json.RawMessage `json:"result"`
		Error         string          `json:"error"`
		Stack         string          `json:"stack"`
		SideEffecting bool            `json:"sideEffecting"`
		DomCoupled    bool            `json:"domCoupled"`
		Note          string          `json:"note"`
		Exports       []string        `json:"exports"`
	}
	_ = json.Unmarshal(rawResult, &parsed)

	data := map[string]any{
		"file_path": a.FilePath,
		"entry":     a.Entry,
		"console":   clines,
	}
	if pathNote != "" {
		data["path_note"] = pathNote
	}
	if parsed.OK {
		switch {
		case parsed.DomCoupled:
			data["dom_coupled"] = true
			data["exports"] = parsed.Exports
			if parsed.Note != "" {
				data["note"] = parsed.Note
			}
		case parsed.SideEffecting:
			data["side_effecting"] = true
			data["exports"] = parsed.Exports
			if parsed.Note != "" {
				data["note"] = parsed.Note
			}
		default:
			data["result"] = parsed.Result
		}
		return tools.Ok(data), nil
	}
	if len(tlines) > 0 {
		data["thrown"] = tlines
	}
	if parsed.Stack != "" {
		data["stack"] = parsed.Stack
	}
	errMsg := parsed.Error
	if errMsg == "" {
		errMsg = "module test returned an unstructured failure (no error message)"
	}
	return &tools.Result{Success: false, Error: errMsg, Data: data}, nil
}

// domMarkers are tokens whose presence means a module reaches into the page DOM or
// canvas, or wires browser events, so it cannot be exercised by calling one export in
// isolation. `window.` is excluded: a classic IIFE that only sets `window.Foo` to
// publish its API is fine to invoke.
var domMarkers = []string{
	"document", "getElementById", "querySelector", "getContext",
	"requestAnimationFrame", "addEventListener", "createElement",
	"HTMLCanvasElement",
}

// moduleReferencesDOM reports whether a module body touches the DOM/canvas or browser
// events. A coarse substring scan on purpose: a false positive only skips invoking that
// export headless (the page-level gate still covers it), never a false pass.
func moduleReferencesDOM(body string) bool {
	for _, m := range domMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// withAwaitPromise opts chromedp.Evaluate into awaiting the returned promise
// so async functions resolve before chromedp serialises the result.
var withAwaitPromise = func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// maxConsoleErrorsReported caps how many distinct console.error/assert lines a single
// render probe carries back, so a page that logs every frame can't bloat the result.
// Distinct lines only (duplicates folded).
const maxConsoleErrorsReported = 12

// maxExceptionChars is the raw budget for ONE uncaught exception (message + stack).
// Wider than the 300-char console/asset clip because the stack is the diagnosis: a
// hard clip keeps only vendor-internal frames and misdirects the fix.
const maxExceptionChars = 1200

// pageRenderReport is what a real browser load revealed about a page beyond its
// HTTP status: whether it threw, whether a same-origin script/stylesheet failed
// to load, and whether it painted visible content (not chrome-only / hidden).
type pageRenderReport struct {
	visibleChars   int
	mediaCount     int
	hiddenChars    int    // text present in the DOM but not painted (opacity:0 / display:none / ...)
	hiddenReason   string // dominant reason that text is hidden: "opacity" / "display" / "visibility" / "zero-size"
	exceptions     []string
	firstPrjFrame  string   // "/files/x.js:153 in boot" - first project-code frame of the first exception, "" when the stack is all library-internal
	failedAssets   []string // human-readable "<status> <url>" / "load failed: <url> (<err>)"
	failedURLs     []string // the bare asset URLs, for owner attribution by the caller
	consoleErrors  []string // console.error / console.assert output the page emitted
	bootFailures   []string // generated-code (/files/) errors logged but not thrown - hard fail
	junkText       []string // visible NaN/undefined/Infinity/[object …] leaks the page painted
	canvasPresent  bool     // a <canvas> is in the DOM
	canvasDominant bool     // that canvas covers a large share of the viewport (a canvas-app page)
	canvasSized    bool     // the canvas has a non-zero backing store (width>0 && height>0)
	rafFrames      int      // requestAnimationFrame calls counted during the observation window (render-loop liveness)
}

// failureReason returns a single actionable reason the render is a HARD failure, or
// ("", false) when the page loaded acceptably. It fails only on deterministic,
// reproducible breakage: an uncaught throw or a same-origin script/stylesheet that
// failed to load. It deliberately does NOT fail on blank paint or canvas liveness -
// those are timing-coupled heuristics that trap the agent in a fix loop; both are
// measured and surfaced informationally (see blankRenderWarning) instead.
func (r *pageRenderReport) failureReason() (string, bool) {
	if len(r.exceptions) > 0 {
		return "page threw an uncaught error during load: " + r.exceptions[0], true
	}
	if len(r.failedAssets) > 0 {
		return "page referenced a script or stylesheet that failed to load: " + r.failedAssets[0] + " (check the file is saved and attached to this page)", true
	}
	// A boot/subsystem failure the project's own code CAUGHT and logged never surfaces
	// as an uncaught throw, so the page still paints and a dead feature ships looking
	// healthy. When generated code (a /files/ frame) logs an Error, treat it as the
	// hard failure it is; a library-originated console.error stays non-fatal.
	if len(r.bootFailures) > 0 {
		return "page logged a fatal error from its own code during load (a caught-and-logged boot/subsystem failure that never threw, so it does not surface as an uncaught error but still leaves the feature dead): " + r.bootFailures[0], true
	}
	return "", false
}

// blankRenderWarning returns an advisory (non-failing) warning when a page served 2xx,
// threw nothing, yet painted nothing visible, or "" when it showed content. Blank is
// not a hard failure (see failureReason) but must not pass silently. When the DOM holds
// text that never painted (hiddenChars > 0), the cause is pinpointed: almost always a
// reveal animation whose JS never ran on this page.
func (r *pageRenderReport) blankRenderWarning() string {
	if r.visibleChars > 0 || r.mediaCount > 0 {
		return ""
	}
	if r.hiddenChars > 0 {
		reason := r.hiddenReason
		if reason == "" {
			reason = "opacity/display"
		}
		return fmt.Sprintf("the page painted 0 visible characters, but %d characters of text are present in the DOM and hidden (%s) - effectively blank to a user. "+
			"This is almost always a reveal animation or shell whose JS never ran on THIS page: an element left at opacity:0 awaiting an .is-visible/IntersectionObserver toggle that lives in a script not attached here, or a layout:\"none\" page missing its own reveal logic. "+
			"Do NOT dismiss this as \"before paint\" - the probe already waited for paint. Fix the cause (ensure the reveal runs, or make the content visible without per-page JS) and re-run test_page before completing.", r.hiddenChars, reason)
	}
	return "the page painted 0 visible characters and 0 media after waiting for paint - it is blank to a user. " +
		"If this page is meant to show content, investigate (a render that never ran, an empty data state with no styled placeholder, or auth-gated content with no visible gate/redirect UI) and re-run test_page. Do not rationalize a blank page as expected."
}

// junkTextWarning returns an advisory when the page painted visible text containing
// NaN / undefined / Infinity / a stringified object: the fingerprint of a math or
// data-binding bug that throws nothing, so neither the exception probe nor
// blankRenderWarning catches it. (A NaN reaching a camera/transform also blacks a
// WebGL canvas silently, with a HUD "NaN" the only signal.) Text-based, so it does
// not false-positive a correct scene; advisory, not a hard fail, but never silent.
func (r *pageRenderReport) junkTextWarning() string {
	if len(r.junkText) == 0 {
		return ""
	}
	return "the page painted visible text containing a NaN/undefined/Infinity or stringified-object leak: " +
		strings.Join(r.junkText, " | ") +
		". This almost always means a math or data-binding bug (and a NaN reaching a camera/transform also renders a WebGL canvas black while throwing nothing). Trace the value to its source, fix it, and re-run test_page - do not ship a screen that prints NaN/undefined."
}

// stuckBootWarning returns an advisory (non-failing) warning when a canvas-app page
// mounted a full-viewport <canvas> but its render loop never ran a frame: the
// fingerprint of a boot that stalled after mounting the canvas. For such a page
// visibleChars counts only the loading/HUD overlay, so it passes every other check
// while the game never starts. Advisory, not a hard fail (a one-shot static canvas
// legitimately runs no frames); fires only when a canvas dominates the viewport AND
// no frame ran, so a text page never triggers it.
func (r *pageRenderReport) stuckBootWarning() string {
	if !r.canvasDominant || r.rafFrames >= 2 {
		return ""
	}
	return fmt.Sprintf("the page served 200 and threw nothing, and a full-viewport <canvas> is mounted, "+
		"but no animation frame ran during the observation window (rafFrames=%d) - the render/game loop never started. "+
		"This is the fingerprint of a boot that stalled after mounting the canvas (e.g. handlers registered against a "+
		"not-yet-connected socket, or an init step gated on a connection that never completed). visible_chars here counts "+
		"only the loading/HUD overlay and is NOT a health signal for this page. Confirm the page actually enters its "+
		"running state (the loop advances, the loading overlay clears), then re-run test_page - do not ship a canvas that never draws a frame.", r.rafFrames)
}

// consoleFromProjectCode reports whether a console call originated in generated
// project code (a script under /files/) rather than a library/CDN frame. It checks
// the call's stack frames first, then the serialized message (an Error arg's stack
// often carries the /files/ URL too). This separates a real project defect from
// library console noise, which the platform leaves for the model to judge.
func consoleFromProjectCode(e *runtime.EventConsoleAPICalled, line string) bool {
	if e.StackTrace != nil {
		for _, f := range e.StackTrace.CallFrames {
			if strings.Contains(f.URL, "/files/") {
				return true
			}
		}
	}
	return strings.Contains(line, "/files/")
}

// browserPoolPartition scopes browser reuse to one project, so two projects that
// resolve to the same local origin never share a Chrome process (nor its cookies,
// localStorage, service workers, or HTTP cache). A zero project id (tests) shares
// one bucket.
func browserPoolPartition(ctx *tools.ToolContext) string {
	if ctx == nil || ctx.ProjectID == 0 {
		return ""
	}
	return fmt.Sprintf("proj-%d", ctx.ProjectID)
}

// verifyPageRender loads path in a real browser and reports what it painted,
// catching pages that pass an HTTP status check but show nothing. Returns
// store.ErrBrowserUnavailable (verbatim) when Chrome cannot launch so the caller
// can fall back to the status-only result rather than fail on a browser-less host.
func verifyPageRender(ctx *tools.ToolContext, domain string, port int, path, authToken string) (*pageRenderReport, error) {
	parentCtx, cancelParent := context.WithTimeout(ctx.Context(), 20*time.Second)
	defer cancelParent()

	allocCtx, releaseBrowser, err := store.AcquireBrowser(parentCtx, store.BrowserConfig{HostResolverMap: domain, PoolPartition: browserPoolPartition(ctx)})
	if err != nil {
		return nil, err
	}
	defer releaseBrowser()

	// The pooled allocator is shared, so the tab (not the browser) carries this
	// call's cancellation.
	tabCtx, cancelTab := store.NewPooledTab(parentCtx, allocCtx)
	defer cancelTab()

	var (
		mu            sync.Mutex
		exceptions    []string
		firstPrjFrame string
		failed        []string
		failedURLs    []string
		consoleErrors []string
		bootFailures  []string
		seenConsole   = map[string]bool{}
		seenBoot      = map[string]bool{}
		reqURLs       = map[network.RequestID]string{}
	)
	clip := func(s string) string { return stringutil.Truncate(s, 300) }
	chromedp.ListenTarget(tabCtx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventExceptionThrown:
			if e.ExceptionDetails != nil {
				mu.Lock()
				// Exceptions get a wider budget than console lines: the stack is the
				// diagnosis, and a hard clip would leave only vendor-internal frames.
				exceptions = append(exceptions, analysis.FormatExceptionForModel(e.ExceptionDetails, maxExceptionChars))
				if firstPrjFrame == "" {
					firstPrjFrame = analysis.FirstProjectFrame(e.ExceptionDetails)
				}
				mu.Unlock()
			}
		case *runtime.EventConsoleAPICalled:
			// console.error / console.assert is how three.js, React and most libraries
			// report misuse that does NOT throw. Capture these so the build agent can
			// judge whether they mean breakage; the platform surfaces, the model judges.
			// Dedup and cap so a spamming page can't bloat the result. Warnings are
			// normally dropped, but inspected here so the swallow-and-warn boot-failure
			// pattern can be promoted below.
			isErrLike := e.Type == runtime.APITypeError || e.Type == runtime.APITypeAssert
			if !isErrLike && e.Type != runtime.APITypeWarning {
				break
			}
			var parts []string
			hasErrorArg := false
			for _, ag := range e.Args {
				if ag == nil {
					continue
				}
				if string(ag.Subtype) == "error" {
					hasErrorArg = true // an Error object was logged, not just text
				}
				if len(ag.Value) > 0 {
					parts = append(parts, strings.Trim(string(ag.Value), `"`))
				} else if ag.Description != "" {
					parts = append(parts, ag.Description)
				}
			}
			line := clip(strings.TrimSpace(strings.Join(parts, " ")))
			if line == "" {
				break
			}
			fromProject := consoleFromProjectCode(e, line)
			mu.Lock()
			switch {
			case fromProject && (isErrLike || hasErrorArg):
				// Generated code logged an error (from /files/) - a real defect the page
				// caught instead of throwing. Promote to a hard failure (see failureReason).
				if !seenBoot[line] && len(bootFailures) < maxConsoleErrorsReported {
					seenBoot[line] = true
					bootFailures = append(bootFailures, line)
				}
			case isErrLike:
				// error/assert from a library or unknown frame: surface, don't auto-fail.
				if !seenConsole[line] && len(consoleErrors) < maxConsoleErrorsReported {
					seenConsole[line] = true
					consoleErrors = append(consoleErrors, line)
				}
			}
			// A bare warning with no Error arg and no /files/ origin is benign: dropped.
			mu.Unlock()
		case *network.EventRequestWillBeSent:
			// Track request URLs so a later LoadingFailed (which carries only a RequestID)
			// can still name the asset that failed - needed to attribute a 404 to its
			// owning task.
			if e.Request != nil {
				mu.Lock()
				reqURLs[e.RequestID] = e.Request.URL
				mu.Unlock()
			}
		case *network.EventResponseReceived:
			if e.Response != nil && e.Response.Status >= 400 &&
				(e.Type == network.ResourceTypeScript || e.Type == network.ResourceTypeStylesheet) &&
				ownAssetURL(e.Response.URL, domain) {
				mu.Lock()
				failed = append(failed, fmt.Sprintf("%d %s", int(e.Response.Status), clip(e.Response.URL)))
				failedURLs = append(failedURLs, e.Response.URL)
				mu.Unlock()
			}
		case *network.EventLoadingFailed:
			if e.Type == network.ResourceTypeScript || e.Type == network.ResourceTypeStylesheet {
				mu.Lock()
				url := reqURLs[e.RequestID]
				if url != "" && !ownAssetURL(url, domain) {
					// A third-party CDN failing (offline host, blocked) is not a defect in the
					// project's own files, and the build gate treats it as a warning too.
					mu.Unlock()
					break
				}
				if url != "" {
					failed = append(failed, fmt.Sprintf("load failed: %s (%s)", clip(url), e.ErrorText))
					failedURLs = append(failedURLs, url)
				} else {
					failed = append(failed, "load failed: "+clip(e.ErrorText))
				}
				mu.Unlock()
			}
		}
	})

	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	if domain != "" {
		origin = fmt.Sprintf("http://%s:%d", domain, port)
	}

	runCtx, cancelRun := context.WithTimeout(tabCtx, 12*time.Second)
	defer cancelRun()

	if err := chromedp.Run(runCtx,
		network.Enable(),
		runtime.Enable(),
		// Inject the same envelope tripwire the authoritative gate injects and idle the
		// same window after navigate, so this probe observes what the gate would: a
		// deferred init / first-frame throw that fires after first paint is caught here
		// too. The auth token is empty (an anonymous visitor) unless signed_in was asked for.
		store.RuntimeInstrumentationAction(authToken),
		chromedp.Navigate(origin+path),
		chromedp.Sleep(store.RuntimeIdleWindow),
	); err != nil {
		return nil, fmt.Errorf("render probe failed: %w", err)
	}
	// Poll for a settled paint rather than measuring once: a cold ESM page can paint its
	// first frame a few hundred ms after load, so a single sample can read empty early.
	// Polling returns the moment something shows. Informational only, never an auto-fail.
	vc, err := pollVisibleContent(runCtx, 2500*time.Millisecond, 250*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("render probe failed: %w", err)
	}

	// Read back any envelope-field tripwires the instrumentation recorded and fold them
	// into the hard boot failures, exactly as the gate does. Best-effort.
	var probeViolations []string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`window.__ofProbeViolations || []`, &probeViolations))

	// Canvas + render-loop liveness. A canvas-app page paints almost no visible text, so
	// visibleChars is a false health signal; the honest signal is whether the render loop
	// ran (__ofRAF, the requestAnimationFrame count). No pixel/WebGL readback (that
	// false-positives a correct dark scene); reads a counter and geometry only. Best-effort.
	var live canvasLiveness
	_ = chromedp.Run(runCtx, chromedp.Evaluate(canvasLivenessProbeJS, &live))

	mu.Lock()
	defer mu.Unlock()
	boot := append([]string(nil), bootFailures...)
	for _, v := range probeViolations {
		boot = append(boot, "envelope misuse: "+v)
	}
	return &pageRenderReport{
		visibleChars:   vc.VisibleChars,
		mediaCount:     vc.MediaCount,
		hiddenChars:    vc.HiddenChars,
		hiddenReason:   vc.HiddenReason,
		exceptions:     append([]string(nil), exceptions...),
		firstPrjFrame:  firstPrjFrame,
		failedAssets:   append([]string(nil), failed...),
		failedURLs:     append([]string(nil), failedURLs...),
		consoleErrors:  append([]string(nil), consoleErrors...),
		bootFailures:   boot,
		junkText:       append([]string(nil), vc.JunkText...),
		canvasPresent:  live.CanvasPresent,
		canvasDominant: live.CanvasDominant,
		canvasSized:    live.CanvasSized,
		rafFrames:      live.RAFFrames,
	}, nil
}

// canvasLiveness is the informational canvas/render-loop measurement (no pixel
// readback - see canvasLivenessProbeJS).
type canvasLiveness struct {
	CanvasPresent  bool `json:"canvasPresent"`
	CanvasDominant bool `json:"canvasDominant"`
	CanvasSized    bool `json:"canvasSized"`
	RAFFrames      int  `json:"rafFrames"`
}

// canvasLivenessProbeJS reports whether the page is a canvas app and whether its
// render loop ran. canvasDominant means a single <canvas> covers a large share of the
// viewport (visibleChars is meaningless there); rafFrames is the requestAnimationFrame
// count. Reads geometry and a counter only, never pixels, so it can't false-positive.
const canvasLivenessProbeJS = `(function () {
  var c = document.querySelector('canvas');
  var r = c && c.getBoundingClientRect();
  var vp = (window.innerWidth * window.innerHeight) || 1;
  return {
    canvasPresent:  !!c,
    canvasDominant: !!(r && (r.width * r.height) > 0.35 * vp),
    canvasSized:    !!(c && c.width > 0 && c.height > 0),
    rafFrames:      window.__ofRAF || 0
  };
})()`

// visibleContentProbeJS measures what a page painted: visible text length OUTSIDE the
// page chrome plus a count of visible media. It walks each node's ancestor chain so
// content hidden by display/visibility/near-zero opacity is tallied separately as
// hiddenChars (pinpointing a 200-but-blank page from an unrun reveal). Informational
// only, never an auto-fail. Canvas liveness is not inspected (pixel readback
// false-positives correct scenes).
const visibleContentProbeJS = `(() => {
  const EXCLUDE = new Set(['NAV','HEADER','FOOTER','SCRIPT','STYLE','NOSCRIPT','TEMPLATE']);
  function inChrome(el){ for (let e=el; e; e=e.parentElement){ if (EXCLUDE.has(e.tagName)) return true; } return false; }
  // hiddenReason returns '' when el is visible, else the first reason it is not,
  // walking the ancestor chain (so a hidden parent hides its descendants).
  function hiddenReason(el){
    for (let e=el; e; e=e.parentElement){
      const cs = getComputedStyle(e);
      if (cs.display === 'none') return 'display';
      if (cs.visibility === 'hidden') return 'visibility';
      if (parseFloat(cs.opacity || '1') < 0.05) return 'opacity';
    }
    const r = el.getBoundingClientRect();
    if (!(r.width > 1 && r.height > 1)) return 'zero-size';
    return '';
  }
  // Tokens that should never reach the screen: a NaN/undefined/Infinity leak or a
  // stringified object is the fingerprint of a math or data-binding bug. A NaN in a
  // transform also blacks a WebGL canvas while throwing nothing, so a HUD that prints
  // "NaN" is often the ONLY visible signal. Word-bounded so "Renault"/"fundefined"
  // don't trip it. Collected from VISIBLE text only.
  const JUNK_RE = /\b(?:NaN|undefined|Infinity)\b|\[object [A-Za-z]+\]/;
  let visibleChars = 0, hiddenChars = 0;
  const hiddenByReason = {};
  const junk = [], junkSeen = {};
  if (document.body) {
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    let n;
    while ((n = walker.nextNode())) {
      const t = (n.textContent || '').trim();
      if (!t) continue;
      const el = n.parentElement;
      if (!el || inChrome(el)) continue;
      const reason = hiddenReason(el);
      if (!reason) {
        visibleChars += t.length;
        const m = t.match(JUNK_RE);
        if (m && junk.length < 8 && !junkSeen[m[0]]) { junkSeen[m[0]] = 1; junk.push(t.length > 60 ? t.slice(0, 60) + '…' : t); }
        continue;
      }
      hiddenChars += t.length;
      hiddenByReason[reason] = (hiddenByReason[reason] || 0) + t.length;
    }
  }
  let dominant = '', dominantN = 0;
  for (const k in hiddenByReason){ if (hiddenByReason[k] > dominantN){ dominantN = hiddenByReason[k]; dominant = k; } }
  let mediaCount = 0;
  document.querySelectorAll('img,canvas,svg,video').forEach(function (el) {
    if (!inChrome(el) && !hiddenReason(el)) mediaCount++;
  });
  return { visibleChars: visibleChars, mediaCount: mediaCount, hiddenChars: hiddenChars, hiddenReason: dominant, junkText: junk };
})()`

// visibleContent is the informational paint measurement (no canvas liveness).
type visibleContent struct {
	VisibleChars int      `json:"visibleChars"`
	MediaCount   int      `json:"mediaCount"`
	HiddenChars  int      `json:"hiddenChars"`
	HiddenReason string   `json:"hiddenReason"`
	JunkText     []string `json:"junkText"` // visible NaN/undefined/Infinity/[object …] leaks
}

// pollVisibleContent re-measures visible content until the page shows something or the
// budget elapses, returning the last reading (a cold ESM page can paint its first frame
// a few hundred ms after load). Informational only - never auto-fails the page.
func pollVisibleContent(ctx context.Context, budget, interval time.Duration) (visibleContent, error) {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	deadline := time.Now().Add(budget)
	var last visibleContent
	for {
		var raw json.RawMessage
		if err := chromedp.Run(ctx, chromedp.Evaluate(visibleContentProbeJS, &raw)); err != nil {
			return last, err
		}
		_ = json.Unmarshal(raw, &last)
		if last.VisibleChars > 0 || last.MediaCount > 0 || !time.Now().Before(deadline) {
			return last, nil
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return last, ctx.Err()
		}
	}
}

// ownAssetURL reports whether an asset URL is served by the project itself (its domain,
// or the loopback address the probe reaches it on). An empty or unparseable URL counts
// as the project's own, so a failure that cannot be attributed is still reported.
func ownAssetURL(rawURL, domain string) bool {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return host == strings.ToLower(domain) || host == "127.0.0.1" || host == "localhost" || host == "::1"
}
