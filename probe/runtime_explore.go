package probe

// Explore: the behavioural half of the runtime probe. The load probe and the
// dead-control census never click, so a button whose handler runs but changes
// nothing, a handler that throws, a control another element sits on top of, and a
// page that scrolls sideways on a phone all ship green. Explore loads each page,
// clicks every visible control once (one per distinct control, so ten "Delete"
// buttons in a list cost one click), and watches what each click did: a DOM change,
// a request, a navigation, a dialog, sound, a scroll, or a canvas repaint. It also
// checks the layout at desktop and phone sizes.
//
// Clicks are real (CDP mouse events at the control's centre), so they place orders
// and delete rows: the caller runs Explore inside a data snapshot and restores it
// afterwards. FP bias as in the rest of the probe: anything uncertain reads as
// "something happened".
//
// The verdicts (clickChanged, exploreIssues) are pure and unit-tested; the Chrome
// plumbing around them is covered by the browser tests.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"openfabrica/store"
)

const (
	// exploreMaxClicks bounds the clicks per page; distinct controls beyond it are
	// left unclicked rather than stretching the sweep.
	exploreMaxClicks = 40
	// exploreSettle is how long a click gets to show its effect before the state is
	// read back. Long enough for a fetch to start and a class toggle to land.
	exploreSettle = 450 * time.Millisecond
	// explorePageTimeout caps one page's whole exploration (both viewports).
	explorePageTimeout = 120 * time.Second
	// exploreMinText is the visible text below which a page with no media reads blank.
	exploreMinText = 20
)

// exploreState is what the page looked like at one instant, as far as a click's
// effect is concerned. Counters only grow, so a click changed something exactly
// when any of them moved.
type exploreState struct {
	Mut    int     `json:"mut"`  // DOM mutations (child list, attributes, text)
	Req    int     `json:"req"`  // fetch / XHR / WebSocket sends
	Effect int     `json:"fx"`   // dialogs, window.open, print, sound, clipboard
	Href   string  `json:"href"` // location, so a navigation or hash change counts
	SX     float64 `json:"sx"`
	SY     float64 `json:"sy"`
	Canvas string  `json:"canv"` // cheap signature of visible 2D canvases
	Vals   string  `json:"vals"` // signature of form control values and checked state
	NCanv  int     `json:"ncv"`  // visible canvases; any of them may be WebGL, which no cheap signature reads
	Shot   string  `json:"-"`    // pixel hash of the viewport, taken only when the page has a canvas
}

// clickChanged reports whether anything a user could notice happened between before
// and after.
func clickChanged(before, after exploreState) bool {
	return after.Mut > before.Mut || after.Req > before.Req || after.Effect > before.Effect ||
		after.Href != before.Href || after.SX != before.SX || after.SY != before.SY ||
		after.Canvas != before.Canvas || after.Vals != before.Vals || after.Shot != before.Shot
}

// exploreControl is one clickable control found on the page.
type exploreControl struct {
	Index int    `json:"i"`
	Tag   string `json:"tag"`
	Text  string `json:"text"`
	Sel   string `json:"sel"`
	Key   string `json:"key"`
}

// describe names a control for a report: its tag and label, or its selector.
func (c exploreControl) describe() string {
	if c.Text != "" {
		return fmt.Sprintf("%s %q (%s)", c.Tag, c.Text, c.Sel)
	}
	return c.Tag + " " + c.Sel
}

// clickError is a click that threw.
type clickError struct {
	Control exploreControl
	Errors  []string
}

// coveredControl is a control another element sits on top of.
type coveredControl struct {
	Control  exploreControl
	Viewport string
	Cover    string
}

// overflowReport is a page the user can scroll sideways.
type overflowReport struct {
	Viewport  string
	Width     int
	Scroll    int
	Offenders []string
}

// pageReport collects what exploring one page found, before it is turned into issues.
type pageReport struct {
	Page     string
	Inert    []exploreControl
	Errors   []clickError
	Covered  []coveredControl
	Overflow []overflowReport
	Blank    bool
	Clicked  int
	// Canvas is true when the page draws on a canvas. Whatever a control does to a
	// canvas or WebGL scene is invisible to a DOM census, so an unchanged page is not
	// proof that the click did nothing.
	Canvas bool
	// Redirect is where the page ended up when it is not the page asked for (a login
	// redirect, a not-found fallback). Such a page is explored under its own path, never
	// reported under the one that redirected.
	Redirect string
	// Gate is the label of a first-run layer the explorer dismissed before clicking.
	Gate string
	// LoadFailed is true when the page could not be loaded or threw while loading, so
	// nothing was clicked. The sweep leaves that to the load probe; a caller that
	// reports an explore verdict must not read an unexplored page as a clean one.
	LoadFailed bool
}

// exploreJS installs the page-side observers once and exposes the explore API as
// window.__ofx. Counters cover every channel a working control usually acts
// through; dialogs and window.open are stubbed so a click never blocks or leaves
// the tab (confirm answers yes, the snapshot undoes what that deletes).
//
//go:embed js/explore.js
var exploreJS string

// layoutScan is the payload of window.__ofx.layout().
type layoutScan struct {
	VW      int      `json:"vw"`
	SW      int      `json:"sw"`
	Scrolls bool     `json:"scrolls"`
	Off     []string `json:"off"`
	Text    int      `json:"text"`
	Media   int      `json:"media"`
}

// aimResult is the payload of window.__ofx.aim(i).
type aimResult struct {
	Gone  bool    `json:"gone"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Cover string  `json:"cover"`
}

// RunExplore clicks through and lays out every published page in headless Chrome.
// Same contract as RunRuntimeValidation: no base URL, no browser, or no pages is a
// skip or a no-op, never a failed build. The caller owns the data snapshot.
func RunExplore(ctx context.Context, db *sql.DB, baseURL, hostHeader string) RuntimeResult {
	if baseURL == "" {
		return RuntimeResult{Skipped: "no base URL configured"}
	}
	pages := loadPagePaths(db)
	if len(pages) == 0 {
		return RuntimeResult{}
	}
	allocCtx, cancelAlloc, err := store.LaunchBrowser(ctx, store.BrowserConfig{HostResolverMap: hostHeader})
	if err == nil {
		allocCtx, cancelAlloc, err = startSharedFrom(allocCtx, cancelAlloc)
	}
	if err != nil {
		return RuntimeResult{Skipped: fmt.Sprintf("headless browser unavailable: %v", err)}
	}
	defer cancelAlloc()
	origin := baseURL
	if hostHeader != "" {
		origin = replaceHostInURL(baseURL, hostHeader)
	}
	authToken := authTokenFromContext(ctx)

	// Pages run one after another: clicks write data, and two pages clicking at once
	// would see each other's rows appear mid-observation.
	var issues []ValidationIssue
	for _, p := range pages {
		if ctx.Err() != nil {
			break
		}
		url := origin + concreteProbePath(db, p)
		rep := exploreWithFallback(allocCtx, url, p, authToken)
		issues = append(issues, exploreIssues(rep)...)
	}
	return RuntimeResult{Issues: issues}
}

// exploreWithFallback explores a page as the signed-in user and, when that user is bounced
// away from it (a sign-in or register page redirects someone who already has a session),
// explores it again as an anonymous visitor, which is who the page is for. A page that
// redirects for both is reported as redirected, so nothing is clicked under the wrong path.
func exploreWithFallback(alloc context.Context, url, pagePath, authToken string) pageReport {
	rep := explorePage(alloc, url, pagePath, authToken)
	if rep.Redirect != "" && authToken != "" {
		if anon := explorePage(alloc, url, pagePath, ""); anon.Redirect == "" {
			return anon
		}
	}
	return rep
}

// explorePage explores one page at desktop size (clicks and layout) and at phone
// size (layout and covered controls, no clicks). A page that fails to load is left
// to the load probe, which already reports it.
func explorePage(alloc context.Context, url, pagePath, authToken string) pageReport {
	rep := pageReport{Page: pagePath}
	tabCtx, cancelTab := store.NewIsolatedTab(alloc)
	defer cancelTab()
	ctx, cancel := context.WithTimeout(tabCtx, explorePageTimeout)
	defer cancel()

	errs := listenPageErrors(tabCtx, url)
	// load navigates and installs the observers. A load that throws reports false:
	// that is the load probe's finding, and a page that broke on load is not judged.
	load := func(viewport string) bool {
		errs.take()
		if err := chromedp.Run(ctx,
			viewportAction(viewport),
			chromedp.Navigate(url),
			chromedp.Sleep(store.RuntimeIdleWindow),
		); err != nil {
			return false
		}
		if hard, _ := splitHardSoft(dropAborted(filterEnvironmentalNoise(errs.take()))); len(hard) > 0 {
			return false
		}
		var ok string
		if chromedp.Run(ctx, chromedp.Evaluate(exploreJS, &ok)) != nil || ok != "ok" {
			return false
		}
		// A redirected page is judged by the caller before anything is clicked.
		if final := finalPath(ctx); final == "" || samePath(final, url) {
			// First-run gates (an intro card with a start button) are dismissed the way
			// a user would, so the controls behind them are judged and a deliberate gate
			// is never reported as covering them.
			if g := dismissGates(ctx); g != "" {
				rep.Gate = g
			}
		}
		return true
	}

	if err := chromedp.Run(ctx, runtime.Enable(), network.Enable(), store.RuntimeInstrumentationAction(authToken), seedRandomAction("explore|"+pagePath)); err != nil {
		rep.LoadFailed = true
		return rep
	}
	if !load("desktop") {
		rep.LoadFailed = true
		return rep
	}
	if final := finalPath(ctx); final != "" && !samePath(final, url) {
		rep.Redirect = final
		return rep
	}
	if ov, blank, ok := readLayout(ctx); ok {
		if ov != nil {
			ov.Viewport = "desktop"
			rep.Overflow = append(rep.Overflow, *ov)
		}
		rep.Blank = blank
	}
	if rep.Blank {
		return rep // nothing to click on a blank page; the finding says it all
	}

	done := map[string]bool{}
	dirty := false   // a click changed the page since it was last loaded
	retried := false // the current control was already retried on a fresh load
	for rep.Clicked < exploreMaxClicks && ctx.Err() == nil {
		ctl, found := nextControl(ctx, done)
		if !found {
			break
		}
		var aim aimResult
		if !evalJSON(ctx, fmt.Sprintf("window.__ofx.aim(%d)", ctl.Index), &aim) || aim.Gone {
			done[ctl.Key] = true
			continue
		}
		if aim.Cover != "" {
			// After earlier clicks an open modal or menu covers the page legitimately;
			// judge the control on a fresh load before calling it covered.
			if dirty && !retried {
				retried = true
				if !load("desktop") {
					break
				}
				dirty = false
				continue
			}
			rep.Covered = append(rep.Covered, coveredControl{Control: ctl, Viewport: "desktop", Cover: aim.Cover})
			done[ctl.Key] = true
			retried = false
			continue
		}
		retried = false
		done[ctl.Key] = true

		var before exploreState
		if !readState(ctx, &before) {
			break
		}
		if before.NCanv > 0 {
			rep.Canvas = true
		}
		errs.take()
		if err := chromedp.Run(ctx, chromedp.MouseClickXY(aim.X, aim.Y), chromedp.Sleep(exploreSettle)); err != nil {
			break
		}
		rep.Clicked++
		hard, _ := splitHardSoft(dropAborted(filterEnvironmentalNoise(errs.take())))
		var after exploreState
		navigated := !readState(ctx, &after)
		switch {
		case len(hard) > 0:
			rep.Errors = append(rep.Errors, clickError{Control: ctl, Errors: hard})
		case navigated:
			// The page went away (a link to another page, a logout): that is an effect.
		case !clickChanged(before, after):
			rep.Inert = append(rep.Inert, ctl)
		}
		if navigated || after.Href != before.Href || len(hard) > 0 {
			// Continue from the page itself, freshly loaded.
			if !load("desktop") {
				break
			}
			dirty = false
			continue
		}
		if clickChanged(before, after) {
			dirty = true
		}
	}

	// Phone size: layout and covered controls on a fresh load. No clicks: the
	// handlers are the same code the desktop pass already exercised.
	if ctx.Err() == nil && load("mobile") {
		if ov, _, ok := readLayout(ctx); ok && ov != nil {
			ov.Viewport = "mobile"
			rep.Overflow = append(rep.Overflow, *ov)
		}
		var raw string
		if chromedp.Run(ctx, chromedp.Evaluate("window.__ofx.enumerate()", &raw)) == nil {
			var ctls []exploreControl
			if json.Unmarshal([]byte(raw), &ctls) == nil {
				for _, c := range ctls {
					var aim aimResult
					if evalJSON(ctx, fmt.Sprintf("window.__ofx.aim(%d)", c.Index), &aim) && !aim.Gone && aim.Cover != "" {
						rep.Covered = append(rep.Covered, coveredControl{Control: c, Viewport: "mobile", Cover: aim.Cover})
					}
				}
			}
		}
	}
	return rep
}

// nextControl re-enumerates the page and returns the first control not yet clicked.
// Enumerating each time picks up controls an earlier click revealed (a modal's own
// buttons), which are exactly the ones a load-only probe never sees.
func nextControl(ctx context.Context, done map[string]bool) (exploreControl, bool) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate("window.__ofx.enumerate()", &raw)); err != nil {
		return exploreControl{}, false
	}
	var ctls []exploreControl
	if json.Unmarshal([]byte(raw), &ctls) != nil {
		return exploreControl{}, false
	}
	for _, c := range ctls {
		if !done[c.Key] {
			return c, true
		}
	}
	return exploreControl{}, false
}

// readState reads the page's click-effect counters, plus a pixel hash of the viewport
// when the page has a canvas (a WebGL canvas cannot be read back cheaply, but what it
// shows can be screenshotted). A page that is animating changes every time, which reads
// as "something happened": the bias this probe wants.
func readState(ctx context.Context, st *exploreState) bool {
	if !evalJSON(ctx, "window.__ofx.state()", st) {
		return false
	}
	if st.NCanv > 0 {
		st.Shot = viewportHash(ctx)
	}
	return true
}

// viewportHash is a hash of a low-quality screenshot of the viewport, or "" when none
// could be taken.
func viewportHash(ctx context.Context) string {
	var buf []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		b, err := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(30).Do(c)
		buf = b
		return err
	}))
	if err != nil || len(buf) == 0 {
		return ""
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:8])
}

// finalPath is the pathname the tab is on now.
func finalPath(ctx context.Context) string {
	var p string
	if chromedp.Run(ctx, chromedp.Evaluate("window.__ofx.path()", &p)) != nil {
		return ""
	}
	return p
}

// samePath reports whether the tab's pathname is the one rawURL asked for.
func samePath(final, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	trim := func(p string) string {
		if p = strings.TrimRight(p, "/"); p == "" {
			return "/"
		}
		return p
	}
	return trim(final) == trim(u.Path)
}

// dismissGates clicks the button of a full-viewport first-run layer, up to twice (an
// intro followed by a tutorial card), and returns the label of the first one clicked.
func dismissGates(ctx context.Context) string {
	first := ""
	for i := 0; i < 2; i++ {
		var label string
		if chromedp.Run(ctx, chromedp.Evaluate("window.__ofx.dismissGate()", &label)) != nil || label == "" {
			break
		}
		if first == "" {
			first = label
		}
		_ = chromedp.Run(ctx, chromedp.Sleep(exploreSettle))
	}
	return first
}

// dropAborted removes cancelled requests: navigating away (a sign-out that redirects,
// a form that submits while a fetch is in flight) aborts the request it interrupted,
// which is the browser working, not an error in the page.
func dropAborted(errs []string) []string {
	out := errs[:0:0]
	for _, e := range errs {
		if !strings.Contains(e, "net::ERR_ABORTED") {
			out = append(out, e)
		}
	}
	return out
}

// readLayout returns the horizontal-scroll report (nil when the page fits) and
// whether the page is blank.
func readLayout(ctx context.Context) (*overflowReport, bool, bool) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate("window.__ofx.layout()", &raw)); err != nil {
		return nil, false, false
	}
	var l layoutScan
	if json.Unmarshal([]byte(raw), &l) != nil {
		return nil, false, false
	}
	var ov *overflowReport
	if l.Scrolls {
		ov = &overflowReport{Width: l.VW, Scroll: l.SW, Offenders: l.Off}
	}
	return ov, l.Text < exploreMinText && l.Media == 0, true
}

// evalJSON evaluates an expression that returns a JSON string (or an object) into out.
func evalJSON(ctx context.Context, expr string, out any) bool {
	var raw json.RawMessage
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &raw)); err != nil {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	return json.Unmarshal(raw, out) == nil
}

// exploreIssues turns one page's report into findings. Every message names the page,
// the control and what to do, because the reader is the model that has to fix it.
func exploreIssues(rep pageReport) []ValidationIssue {
	var out []ValidationIssue
	if rep.Blank {
		out = append(out, ValidationIssue{
			Severity:      store.SeverityError,
			Category:      "blank_page",
			Page:          rep.Page,
			Deterministic: true,
			Message: fmt.Sprintf("page %q shows almost nothing after load: under %d characters of visible text and no image, canvas or video. "+
				"Its content is likely never rendered (a mount element that stays empty, data that never arrives, everything hidden until a step that never runs).",
				rep.Page, exploreMinText),
		})
	}
	for _, e := range rep.Errors {
		out = append(out, ValidationIssue{
			Severity:      store.SeverityError,
			Category:      "click_error",
			Page:          rep.Page,
			Deterministic: true,
			Signature:     "click_error|" + e.Control.Sel,
			Message: fmt.Sprintf("clicking %s on page %q throws:\n  - %s\nFix the handler so the click does its job without an error.",
				e.Control.describe(), rep.Page, strings.Join(e.Errors, "\n  - ")),
		})
	}
	if len(rep.Inert) > 0 {
		var named, unnamed []string
		for _, c := range rep.Inert {
			if c.Text != "" {
				named = append(named, c.describe())
			} else {
				unnamed = append(unnamed, c.describe())
			}
		}
		if len(named) > 0 {
			// On a canvas page what a control does to the scene cannot be read from the
			// DOM, so a named control that changed nothing visible is only a warning.
			sev := store.SeverityError
			if rep.Canvas {
				sev = store.SeverityWarning
			}
			out = append(out, inertIssue(rep.Page, sev, named))
		}
		if len(unnamed) > 0 {
			out = append(out, inertIssue(rep.Page, store.SeverityWarning, unnamed))
		}
	}
	for _, c := range rep.Covered {
		out = append(out, ValidationIssue{
			Severity:      store.SeverityError,
			Category:      "covered_control",
			Page:          rep.Page,
			Deterministic: true,
			Signature:     "covered|" + c.Viewport + "|" + c.Control.Sel,
			Message: fmt.Sprintf("on page %q at %s size, %s cannot be clicked: %s sits on top of it, so a tap lands on that element instead. "+
				"Give a decorative covering layer pointer-events:none (restored on its own interactive children), move it, or fix its size or z-index. "+
				"Never do that to a modal, intro or dialog that is meant to block the page: give it a dismiss control that removes it instead.",
				rep.Page, c.Viewport, c.Control.describe(), c.Cover),
		})
	}
	for _, o := range rep.Overflow {
		detail := ""
		if len(o.Offenders) > 0 {
			detail = " Widest elements: " + strings.Join(o.Offenders, "; ") + "."
		}
		out = append(out, ValidationIssue{
			Severity:      store.SeverityError,
			Category:      "layout_overflow",
			Page:          rep.Page,
			Deterministic: true,
			Signature:     "overflow|" + o.Viewport,
			Message: fmt.Sprintf("page %q scrolls sideways at %s size: the content is %dpx wide in a %dpx viewport.%s "+
				"Make those elements shrink (max-width:100%%, min-width:0 on flex/grid children, wrapping, a narrower layout below the breakpoint) instead of clipping the page.",
				rep.Page, o.Viewport, o.Scroll, o.Width, detail),
		})
	}
	return out
}

// inertIssue reports controls whose click changed nothing. Named controls are an
// error: a labelled button promises an action. Unlabelled ones (an icon wrapper, a
// bare checkbox) are a warning.
func inertIssue(page, severity string, controls []string) ValidationIssue {
	sorted := append([]string(nil), controls...)
	sort.Strings(sorted)
	return ValidationIssue{
		Severity:      severity,
		Category:      "inert_click",
		Page:          page,
		Deterministic: severity == store.SeverityError,
		Signature:     "inert|" + severity + "|" + strings.Join(sorted, "|"),
		Message: fmt.Sprintf("on page %q, clicking these controls changed nothing: no DOM update, request, navigation, dialog, sound or scroll: %s. "+
			"Each handler must do its visible job (a reveal must undo exactly what hid the element), or the control must go. "+
			"This check only sees DOM changes, requests, navigation, dialogs, sound, scrolling, form values and canvas pixels: if a control really works by other means (it only drives audio or a 3D scene), leave it alone and say so when completing the item. Never add a decorative animation just to satisfy this check.",
			page, strings.Join(sorted, "; ")),
	}
}

// ExploreSummary is what exploring one page found, for a caller that wants the facts
// next to the findings (the model's own explore action).
type ExploreSummary struct {
	Page     string
	Clicked  int
	Gate     string // first-run layer the explorer dismissed
	Redirect string // where the page ended up, when it is not the page asked for
	Canvas   bool
	// LoadFailed: the page did not load cleanly, so nothing was explored.
	LoadFailed bool
}

// ExplorePageAt explores one page of the running project (clicks at desktop size,
// layout at desktop and phone) and returns its findings. The caller owns the data
// snapshot, as for RunExplore. Same skip contract: no base URL or no browser is a skip.
func ExplorePageAt(ctx context.Context, baseURL, hostHeader, pagePath, authToken string) (RuntimeResult, ExploreSummary) {
	sum := ExploreSummary{Page: pagePath}
	if baseURL == "" {
		return RuntimeResult{Skipped: "no base URL configured"}, sum
	}
	allocCtx, cancelAlloc, err := store.LaunchBrowser(ctx, store.BrowserConfig{HostResolverMap: hostHeader})
	if err == nil {
		allocCtx, cancelAlloc, err = startSharedFrom(allocCtx, cancelAlloc)
	}
	if err != nil {
		return RuntimeResult{Skipped: fmt.Sprintf("headless browser unavailable: %v", err)}, sum
	}
	defer cancelAlloc()
	origin := baseURL
	if hostHeader != "" {
		origin = replaceHostInURL(baseURL, hostHeader)
	}
	rep := exploreWithFallback(allocCtx, origin+pagePath, pagePath, authToken)
	sum.Clicked, sum.Gate, sum.Redirect, sum.Canvas, sum.LoadFailed = rep.Clicked, rep.Gate, rep.Redirect, rep.Canvas, rep.LoadFailed
	return RuntimeResult{Issues: exploreIssues(rep)}, sum
}
