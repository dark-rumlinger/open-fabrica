package probe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/replayid"
	"openfabrica/store"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Flow session lifecycle: isolated tabs, browser flags, dialogs, downloads,
// offline/clock emulation.

func (d *flowDialog) arm(accept bool, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed, d.accept, d.text = true, accept, text
}

func (d *flowDialog) answer(msg, defaultPrompt string) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last = msg
	if !d.armed {
		return true, defaultPrompt
	}
	d.armed = false
	return d.accept, d.text
}

func (d *flowDialog) lastMessage() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// session returns the named session, opening a fresh isolated one on first use.
func (r *flowRun) session(name string) (*flowSession, error) {
	if s, ok := r.sessions[name]; ok {
		return s, nil
	}
	ctx, cancel, tab, bctx, err := newIsolatedTab(r.browserCtx, r.origin, r.fixtures)
	if err != nil {
		return nil, err
	}
	s := &flowSession{name: name, ctx: ctx, cancel: cancel, tab: tab, bctx: bctx, popups: make(chan target.ID, 4),
		snaps: map[string]string{}, workers: map[target.ID]context.Context{}}
	if err := r.setupTab(s, ctx); err != nil {
		cancel()
		return nil, err
	}
	s.errs = listenPageErrors(ctx, r.origin)
	r.sessions[name] = s
	r.order = append(r.order, name)
	return s, nil
}

// setupTab prepares a tab for a session: dialogs answered, popups noticed, the
// replay identity, the load probe's instrumentation, the device, and whatever
// browser state the session's steps set (offline, clock).
func (r *flowRun) setupTab(s *flowSession, ctx context.Context) error {
	// Answer every alert/confirm/prompt the way a user clicking OK would, unless a
	// dialog step said otherwise; an unanswered dialog blocks the page and would
	// hang the rest of the flow.
	chromedp.ListenTarget(ctx, func(ev any) {
		if e, ok := ev.(*page.EventJavascriptDialogOpening); ok {
			accept, text := s.dialog.answer(string(e.Type)+": "+e.Message, e.DefaultPrompt)
			go func() { _ = chromedp.Run(ctx, page.HandleJavaScriptDialog(accept).WithPromptText(text)) }()
		}
	})
	opener := s.tab
	chromedp.ListenBrowser(ctx, func(ev any) {
		if e, ok := ev.(*target.EventTargetCreated); ok && e.TargetInfo != nil && e.TargetInfo.Type == "page" && e.TargetInfo.OpenerID == opener {
			select {
			case s.popups <- e.TargetInfo.TargetID:
			default:
			}
		}
	})
	visitor := s.name
	if visitor == "" {
		visitor = "main"
	}
	// The first Run creates (or attaches to) the tab; no timeout on it (a cancelled
	// first Run tears the target down), the parent context bounds it instead.
	if err := chromedp.Run(ctx,
		runtime.Enable(),
		network.Enable(),
		// The replay identity rides a cookie scoped to the project's origin, never an
		// extra request header: CDP sends extra headers cross-origin too, which turns every
		// CDN module import into a CORS preflight the CDN refuses (see replayid.Cookie).
		network.SetCookie(replayid.Cookie, replayid.Value(r.replayToken, fmt.Sprintf("%d.%s", r.flowIndex, visitor))).
			WithURL(r.origin+"/").WithHTTPOnly(true),
		// Same pre-script instrumentation as the load probe. No seeded token unless
		// the flow declares a signedIn user for this session: otherwise a flow signs
		// in through the real UI.
		store.RuntimeInstrumentationAction(r.seedTokens[s.name]),
		seedRandomAction(fmt.Sprintf("%s|%d|%s", r.flow.Name, r.flowIndex, visitor)),
		viewportAction(r.flow.Viewport),
		// A headless tab is never the focused window; apps that check focus (the
		// clipboard API refuses an unfocused document) behave as in a person's browser.
		emulation.SetFocusEmulationEnabled(true),
		// "Near me" features get a fixed position (central Amsterdam) instead of a
		// permission error; the permission itself is granted per browser context.
		emulation.SetGeolocationOverride().WithLatitude(52.3731).WithLongitude(4.8922).WithAccuracy(25),
	); err != nil {
		return err
	}
	if s.offline {
		if err := chromedp.Run(ctx, offlineAction(true)); err != nil {
			return err
		}
	}
	if s.clockOffset != 0 {
		return r.applyClock(ctx, s)
	}
	return nil
}

// adoptPopup moves a session into the tab its last step opened (a target=_blank
// link, window.open), as a person's attention follows a new tab. Later steps -
// including goto and reload - act in it; the note says so, so a failure there is
// not read as the old page.
func (r *flowRun) adoptPopup(s *flowSession) {
	var tab target.ID
	select {
	case tab = <-s.popups:
	default:
		return
	}
	ctx, cancel := chromedp.NewContext(r.browserCtx, chromedp.WithTargetID(tab))
	prev := s.tab
	s.tab = tab
	if err := r.setupTab(s, ctx); err != nil {
		s.tab = prev
		cancel()
		return
	}
	s.closeTabs = append(s.closeTabs, cancel)
	s.ctx, s.errs = ctx, listenPageErrors(ctx, r.origin)
	var where string
	c, done := context.WithTimeout(ctx, 2*time.Second)
	_ = chromedp.Run(c, chromedp.Evaluate(`location.pathname + location.search`, &where))
	done()
	r.note(fmt.Sprintf("the previous step opened a new tab (%s); %s continues there, as a person would", where, s.name))
}

// newIsolatedTab opens a tab in a brand-new browser context (its own cookies and
// storage: a separate user) and returns a chromedp context attached to it.
// Cancelling the context closes the tab and disposes the browser context. The
// context and target are created by hand because headless Chrome refuses a new tab
// in a windowless context ("no browser is open") unless it is asked for a new
// window, which chromedp's WithNewBrowserContext does not do. The context gets the
// permissions a person grants when asked (camera, microphone, clipboard, location,
// notifications) and saves downloads to the run's fixture directory.
func newIsolatedTab(browserCtx context.Context, origin string, fx *flowFixtures) (context.Context, context.CancelFunc, target.ID, cdp.BrowserContextID, error) {
	var (
		bctx cdp.BrowserContextID
		tid  target.ID
	)
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		exec := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
		var err error
		if bctx, err = target.CreateBrowserContext().WithDisposeOnDetach(true).Do(exec); err != nil {
			return err
		}
		// One grant per permission: a Chrome that does not know one refuses the
		// whole call, and the others still matter.
		for _, name := range flowPermissions {
			desc := &browser.PermissionDescriptor{Name: name, AllowWithoutSanitization: strings.HasPrefix(name, "clipboard")}
			_ = browser.SetPermission(desc, browser.PermissionSettingGranted).WithOrigin(origin).WithBrowserContextID(bctx).Do(exec)
		}
		if dir, derr := fx.downloadDir(); derr == nil {
			_ = browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
				WithBrowserContextID(bctx).WithDownloadPath(dir).WithEventsEnabled(true).Do(exec)
		}
		tid, err = target.CreateTarget("about:blank").WithBrowserContextID(bctx).WithNewWindow(true).Do(exec)
		return err
	})); err != nil {
		return nil, nil, "", "", err
	}
	tabCtx, cancelTab := chromedp.NewContext(browserCtx, chromedp.WithTargetID(tid))
	cancel := func() {
		cancelTab()
		_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			exec := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
			_ = target.CloseTarget(tid).Do(exec)
			return target.DisposeBrowserContext(bctx).Do(exec)
		}))
	}
	return tabCtx, cancel, tid, bctx, nil
}

// flowBrowserFlags are the launch flags a flow browser adds to the shared defaults,
// so apps that use a camera, a microphone, WebRTC or secure-context APIs run in the
// test browser as they do for a person.
func flowBrowserFlags(origin string) []chromedp.ExecAllocatorOption {
	return []chromedp.ExecAllocatorOption{
		// A camera and a microphone that produce a test pattern and a tone, allowed
		// without a prompt: video calls, voice notes and scanners get a real stream.
		chromedp.Flag("use-fake-device-for-media-stream", true),
		chromedp.Flag("use-fake-ui-for-media-stream", true),
		// Two sessions on one machine connect peer to peer over their real loopback
		// addresses; the mDNS names Chrome hides local IPs behind do not resolve here.
		// chromedp's own disable-features list is repeated: the flag holds one value.
		chromedp.Flag("disable-features", "site-per-process,Translate,BlinkGenPropertyTrees,WebRtcHideLocalIpsWithMdns"),
		// A published project is replayed over plain http on its own domain, which is
		// not a secure context: service workers, WebCrypto, the clipboard and
		// getUserMedia would be missing there, while visitors on https have them.
		chromedp.Flag("unsafely-treat-insecure-origin-as-secure", origin),
	}
}

// offlineAction takes the tab's network down (or back up), as losing the
// connection does: requests fail, navigator.onLine flips and offline/online fire.
func offlineAction(off bool) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if _, err := network.EmulateNetworkConditionsByRule(off, []*network.Conditions{{DownloadThroughput: -1, UploadThroughput: -1}}).Do(ctx); err != nil {
			return err
		}
		return network.OverrideNetworkState(off, 0, -1, -1).Do(ctx)
	})
}

// setWorkersOffline gives the session's service workers the same network state as
// its tab: a worker fetches on its own, so taking only the page offline would let a
// network-first worker keep answering from the network and pass as offline-ready.
// Best effort: a worker that cannot be reached keeps its network.
func (r *flowRun) setWorkersOffline(s *flowSession, off bool) {
	var infos []*target.Info
	if err := chromedp.Run(r.browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		infos, err = target.GetTargets().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		return err
	})); err != nil {
		return
	}
	for _, ti := range infos {
		if ti.Type != "service_worker" || ti.BrowserContextID != s.bctx {
			continue
		}
		wctx, ok := s.workers[ti.TargetID]
		if !ok {
			c, cancel := chromedp.NewContext(r.browserCtx, chromedp.WithTargetID(ti.TargetID))
			s.closeTabs = append(s.closeTabs, cancel)
			s.workers[ti.TargetID] = c
			wctx = c
		}
		c, done := context.WithTimeout(wctx, 5*time.Second)
		_ = chromedp.Run(c, offlineAction(off))
		done()
	}
}

// applyClock installs the session's clock offset in the current page and in every
// page it loads from now on.
func (r *flowRun) applyClock(ctx context.Context, s *flowSession) error {
	script := fmt.Sprintf(flowClockJS, s.clockOffset.Milliseconds())
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if s.clockScript != "" {
			_ = page.RemoveScriptToEvaluateOnNewDocument(s.clockScript).Do(ctx)
		}
		id, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		if err != nil {
			return err
		}
		s.clockScript = id
		return chromedp.Evaluate(script, nil).Do(ctx)
	}))
}

func (d *flowDownloads) listen(ev any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.names == nil {
		d.names, d.state, d.bytes = map[string]string{}, map[string]string{}, map[string]float64{}
	}
	switch e := ev.(type) {
	case *browser.EventDownloadWillBegin:
		if _, seen := d.names[e.GUID]; !seen {
			d.order = append(d.order, e.GUID)
		}
		d.names[e.GUID] = e.SuggestedFilename
		if d.state[e.GUID] == "" {
			d.state[e.GUID] = "inProgress"
		}
	case *browser.EventDownloadProgress:
		d.state[e.GUID] = string(e.State)
		d.bytes[e.GUID] = e.ReceivedBytes
	}
}

// count is how many downloads have begun so far.
func (d *flowDownloads) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.order)
}

// after reports the first download that began after the first n: its file name,
// state ("" when none began) and size.
func (d *flowDownloads) after(n int) (name, state string, size float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.order) <= n {
		return "", "", 0
	}
	g := d.order[n]
	return d.names[g], d.state[g], d.bytes[g]
}

// viewportAction emulates the flow's device: desktop is a 1280x800 window; mobile
// and tablet are touch devices in portrait with a phone user agent, so responsive
// layouts, touch handling and UA checks all take their mobile path.
//
// On a touch device clicks and drags are sent as touches (see touchTap), so an
// on-screen joystick or swipe handler that only listens for touch reacts as on a
// phone. Landscape variants are the same devices turned sideways (games, video).
func viewportAction(v string) chromedp.Action {
	switch v {
	case "mobile", "tablet", "mobile-landscape", "tablet-landscape":
		w, h := int64(390), int64(844)
		if strings.HasPrefix(v, "tablet") {
			w, h = 820, 1180
		}
		orient := chromedp.EmulatePortrait
		if strings.HasSuffix(v, "-landscape") {
			w, h = h, w
			orient = chromedp.EmulateLandscape
		}
		return chromedp.Tasks{
			chromedp.EmulateViewport(w, h, chromedp.EmulateMobile, chromedp.EmulateTouch, orient, chromedp.EmulateScale(2)),
			emulation.SetUserAgentOverride(flowMobileUA),
		}
	}
	return chromedp.EmulateViewport(1280, 800)
}
