package probe

// Flow runner: drives the journeys in of_test.js (see flows_parse.go) through a
// real headless Chrome - navigate, type, click, drag, press keys, upload - and
// reports the first step of each flow that fails. A step fails when its action or
// assertion times out, or when project JS throws while it runs; the latter is what a
// page-load probe can never see (a submit handler that crashes, a game loop that
// dies on the first keypress). Flows run sequentially in file order. Each session a
// flow uses ("main", plus any step's `as`) is its own isolated browser profile, so
// every flow starts logged out and two sessions are two different users. Run
// variables are per session and shared across flows, so a later flow can log in as
// the user an earlier one signed up.

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"openfabrica/internal/dbh"
	"openfabrica/internal/flowsnap"
	"openfabrica/internal/replayid"
	"openfabrica/security"
	"openfabrica/store"
)

const (
	// flowTimeout caps one flow end to end, so a hung app cannot eat the stage.
	flowTimeout = 180 * time.Second
	// flowSettle is how long an action step waits for the handlers it triggered to
	// run (and throw) before the next step starts. Assertions poll on their own, so
	// this only needs to cover a synchronous handler plus its first microtasks.
	flowSettle = 350 * time.Millisecond
	// flowPoll is the interval between attempts while a step waits for its target.
	flowPoll = 100 * time.Millisecond
	// flowPixelPoll is the (slower) interval between screenshots for expectChanged.
	flowPixelPoll = 250 * time.Millisecond
	// flowFrameWindow is how long expectAnimating watches requestAnimationFrame, and
	// flowMinFrames how many requests in that window count as a running loop.
	flowFrameWindow = 400 * time.Millisecond
	flowMinFrames   = 3
	// flowTestEmailDomain marks every account a flow registers, so leftovers from a
	// run that died before cleanup are still recognisable (see CleanupFlowUsers).
	flowTestEmailDomain = "example.test"
	// flowMarker tags the element a step resolved, so the Go side can address it
	// with a plain CSS query whatever the original selector was.
	flowMarker = `[data-of-flow-target]`
)

// FlowVars are one session's values for {{email}}, {{password}}, {{username}} and
// {{run}}. Unique per run so a signup never collides with an earlier run's account.
type FlowVars struct {
	Run, Email, Password, Username, Phone string
}

// flowVarSet hands out one FlowVars per session name, created on first use and
// stable for the whole run.
type flowVarSet struct {
	run, secret string
	bySession   map[string]FlowVars
	order       []string
	// remembered holds values captured by remember steps; shared by every session
	// and every later flow in the run.
	remembered map[string]string
}

func newFlowVarSet() *flowVarSet {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	return &flowVarSet{run: id[:8], secret: id[8:], bySession: map[string]FlowVars{}, remembered: map[string]string{}}
}

func (s *flowVarSet) get(session string) FlowVars {
	if v, ok := s.bySession[session]; ok {
		return v
	}
	emailTag, userTag := s.run, "oftest_"+s.run
	if session != defaultSession {
		emailTag += "-" + session
		userTag += "_" + session
	}
	v := FlowVars{
		Run:      s.run,
		Email:    "of-test+" + emailTag + "@" + flowTestEmailDomain,
		Password: "Of-" + s.secret + "!9aZ",
		Username: userTag,
		Phone:    flowPhone(s.run, session),
	}
	s.bySession[session] = v
	s.order = append(s.order, session)
	return v
}

func (s *flowVarSet) all() []FlowVars {
	out := make([]FlowVars, 0, len(s.order))
	for _, name := range s.order {
		out = append(out, s.bySession[name])
	}
	return out
}

// expand substitutes variables for a step running in session. {{var:other}} reads
// another session's value (e.g. main messaging {{username:bob}}).
func (s *flowVarSet) expand(str, session string) string {
	return flowVarRe.ReplaceAllStringFunc(str, func(m string) string {
		sub := flowVarRe.FindStringSubmatch(m)
		if sub[1] == "totp" {
			if secret, ok := s.remembered[sub[2]]; ok {
				return flowTOTP(secret)
			}
			return m
		}
		if !flowVars[sub[1]] {
			if v, ok := s.remembered[sub[1]]; ok {
				return v
			}
			return m
		}
		who := session
		if sub[2] != "" {
			who = sub[2]
		}
		v := s.get(who)
		switch sub[1] {
		case "email":
			return v.Email
		case "password":
			return v.Password
		case "username":
			return v.Username
		case "phone":
			return v.Phone
		case "run":
			return v.Run
		}
		return m
	})
}

// FlowResult is the outcome of a flow run. Skipped is non-empty when no browser
// could be launched; Passed and Failed name the flows by outcome; Accounts are the
// identities the run handed out, for CleanupFlowUsers.
type FlowResult struct {
	Issues  []ValidationIssue
	Skipped string
	Passed  []string
	Failed  []string
	// Blocked names flows that did not run because an earlier flow they take a
	// remembered value from failed before capturing it. Not failures of their own:
	// the earlier flow's failure is the one to fix.
	Blocked  []string
	Accounts []FlowVars
	// WeakChecks are advisory notes about steps that passed without proving much.
	WeakChecks []string
}

// flowWriteDBKey carries a write handle for post-run cleanup: validators receive
// only the read pool, so the build stage threads the writer through the context.
type flowWriteDBKeyT struct{}

// WithFlowWriteDB returns a context from which the flow validator can delete the
// accounts its run registered. A nil handle is a no-op.
func WithFlowWriteDB(ctx context.Context, db dbh.Handle) context.Context {
	if db == nil {
		return ctx
	}
	return context.WithValue(ctx, flowWriteDBKeyT{}, db)
}

// FlowWriteDBFromContext returns the handle set by WithFlowWriteDB, or nil.
func FlowWriteDBFromContext(ctx context.Context) dbh.Handle {
	db, _ := ctx.Value(flowWriteDBKeyT{}).(dbh.Handle)
	return db
}

// launchFlowBrowser starts the browser a flow run uses; a variable so tests can
// simulate a host without Chrome.
var launchFlowBrowser = store.LaunchBrowser

// SetFlowBrowserLauncherForTest swaps the launcher flow runs use and returns a
// restore func. Tests only: it lets packages above probe exercise the no-Chrome path.
func SetFlowBrowserLauncherForTest(fn func(context.Context, store.BrowserConfig) (context.Context, context.CancelFunc, error)) func() {
	prev := launchFlowBrowser
	launchFlowBrowser = fn
	return func() { launchFlowBrowser = prev }
}

// RunFlowTests runs suite against the project served at baseURL (hostHeader is the
// project's vhost, resolved to the local server inside Chrome). When only is
// non-empty, just the flow with that name runs.
func RunFlowTests(ctx context.Context, suite *FlowSuite, baseURL, hostHeader, only string) (res FlowResult) {
	vars := newFlowVarSet()
	defer func() { res.Accounts = vars.all() }()
	if suite == nil || len(suite.Flows) == 0 {
		return res
	}
	if baseURL == "" {
		res.Skipped = "no base URL configured"
		return res
	}
	origin := strings.TrimRight(baseURL, "/")
	if hostHeader != "" {
		origin = replaceHostInURL(baseURL, hostHeader)
	}
	allocCtx, cancelAlloc, err := launchFlowBrowser(ctx, store.BrowserConfig{HostResolverMap: hostHeader, ExtraFlags: flowBrowserFlags(origin)})
	if err != nil {
		res.Skipped = fmt.Sprintf("headless browser unavailable: %v", err)
		return res
	}
	defer cancelAlloc()
	// Start the browser once; every session is a new isolated browser context in it.
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()
	if err := chromedp.Run(browserCtx); err != nil {
		res.Skipped = fmt.Sprintf("headless browser unavailable: %v", err)
		return res
	}
	fx := &flowFixtures{}
	defer fx.cleanup()
	downloads := &flowDownloads{}
	chromedp.ListenBrowser(browserCtx, downloads.listen)
	hooks := flowHooksFrom(ctx)
	if hooks != nil {
		end := hooks.Begin()
		defer end()
	}
	// Every session tab counts against its own rate-limit bucket for as long as this
	// replay runs, like the separate visitor it simulates (see replayid).
	replayToken, endReplay := replayid.Begin()
	defer endReplay()

	producer := rememberProducers(suite.Flows)
	selected := selectFlows(suite.Flows, only, producer)
	for i, f := range suite.Flows {
		if !selected[i] {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if name, ok := missingRemembered(f, vars); ok {
			res.Blocked = append(res.Blocked, fmt.Sprintf("%s (needs {{%s}}, which flow %q failed before remembering)",
				f.Name, name, suite.Flows[producer[name]].Name))
			continue
		}
		r := &flowRun{browserCtx: browserCtx, origin: origin, flow: f, vars: vars, fixtures: fx, downloads: downloads, hooks: hooks,
			sessions: map[string]*flowSession{}, deadline: time.Now().Add(flowTimeout),
			replayToken: replayToken, flowIndex: i}
		iss := r.run()
		res.WeakChecks = append(res.WeakChecks, r.weak...)
		// A browser that dies mid-run, or a caller that cancels the run (the stage
		// timed out), says nothing about the app: report the run as skipped instead
		// of turning the environment's failure into a flow error and a fix-pass.
		if r.envErr != nil {
			res.Skipped = fmt.Sprintf("headless browser failed during flow %q: %v", f.Name, r.envErr)
			res.Issues, res.Passed, res.Failed, res.Blocked = nil, nil, nil, nil
			return res
		}
		if ctx.Err() != nil {
			res.Skipped = fmt.Sprintf("flow run was cancelled before it finished (%v)", ctx.Err())
			res.Issues, res.Passed, res.Failed, res.Blocked = nil, nil, nil, nil
			return res
		}
		if iss != nil {
			res.Issues = append(res.Issues, *iss)
			res.Failed = append(res.Failed, f.Name)
		} else {
			res.Passed = append(res.Passed, f.Name)
		}
	}
	return res
}

// rememberProducers maps each remembered name to the first flow that captures it.
func rememberProducers(flows []Flow) map[string]int {
	producer := map[string]int{}
	for i, f := range flows {
		for _, name := range f.remembers() {
			if _, ok := producer[name]; !ok {
				producer[name] = i
			}
		}
	}
	return producer
}

// selectFlows picks the flows a run executes: all of them, or the named one plus
// every earlier flow it takes remembered values from (transitively), so running one
// flow on its own behaves exactly as it does inside the full suite.
func selectFlows(flows []Flow, only string, producer map[string]int) []bool {
	selected := make([]bool, len(flows))
	if only == "" {
		for i := range selected {
			selected[i] = true
		}
		return selected
	}
	var mark func(i int)
	mark = func(i int) {
		if selected[i] {
			return
		}
		selected[i] = true
		for _, name := range flows[i].rememberedNeeds() {
			if p, ok := producer[name]; ok && p < i {
				mark(p)
			}
		}
	}
	for i, f := range flows {
		if f.Name == only {
			mark(i)
		}
	}
	return selected
}

// missingRemembered reports the first remembered value a flow needs from an earlier
// flow that was never captured.
func missingRemembered(f Flow, vars *flowVarSet) (string, bool) {
	for _, name := range f.rememberedNeeds() {
		if _, ok := vars.remembered[name]; !ok {
			return name, true
		}
	}
	return "", false
}

// flowFailure is why a step failed: its kind feeds the issue signature.
type flowFailure struct {
	kind   string // "js_error" | "not_found" | "covered" | "assert" | "navigation"
	detail string
}

// flowSession is one isolated user: its own browser context, tab and error stream.
type flowSession struct {
	name   string
	ctx    context.Context
	cancel context.CancelFunc
	errs   *pageErrorCollector
	snaps  map[string]string // selector -> screenshot hash, for expectChanged
	// drifts marks selectors that change on their own, so expectChanged on them proves nothing.
	drifts map[string]bool
	// tab is the target the session drives; popups carries tabs it opened
	// (target=_blank, window.open), which the session moves into like a person does.
	tab       target.ID
	bctx      cdp.BrowserContextID
	popups    chan target.ID
	closeTabs []context.CancelFunc
	workers   map[target.ID]context.Context // attached service workers (offline)
	dialog    flowDialog
	// Browser state steps set, re-applied to a tab the session moves into.
	offline     bool
	clockOffset time.Duration
	clockScript page.ScriptIdentifier
	// What the session's last request/webhook got back and the last message an
	// expectEmail found, for expectResponse and remember.
	lastResp *flowResponse
	lastMsg  *FlowMessage
}

// flowDialog decides how the session answers alert, confirm and prompt: accept
// (with the prompt's default) unless a dialog step armed another answer for the
// next one. last is the most recent dialog's text, for failure explanations.
type flowDialog struct {
	mu     sync.Mutex
	armed  bool
	accept bool
	text   string
	last   string
}

type flowRun struct {
	browserCtx context.Context
	origin     string
	flow       Flow
	vars       *flowVarSet
	fixtures   *flowFixtures
	downloads  *flowDownloads
	hooks      FlowHooks
	// seedTokens are the session tokens of the flow's signedIn users, by session.
	seedTokens map[string]string
	sessions   map[string]*flowSession
	order      []string
	// recentFails holds the failed requests of the last few steps: the request that
	// causes a failure often fails a step or two before the step that notices it
	// (a click sends it, the next step waits for its result).
	recentFails []httpFail
	// recentCalls holds the API responses of the last few steps, any status, so a red
	// step shows what the server returned without a console.log and a second replay.
	recentCalls []httpFail
	// scriptFails are the scripts that failed to load from another origin (a CDN
	// library) during this flow. They classify as soft - the project cannot fix a CDN -
	// but a module the entry imports failing to load means none of the page's JS ran,
	// so a red step names them instead of reporting that nothing errored.
	scriptFails []string
	deadline    time.Time
	// envErr is set when the browser itself failed (a session could not be
	// opened): an environment problem, never a verdict on the app.
	envErr error
	// replayToken and flowIndex name each session's simulated visitor to the
	// server's rate limiters (replayid): one bucket per flow and session.
	replayToken string
	flowIndex   int
	// stepIdx is the step running now; notes are what earlier steps did differently
	// from what the script said (a value a field's maxlength cut short), printed
	// with a failure because they are often its cause.
	stepIdx int
	notes   []string
	// weak collects advisory notes about steps that passed without proving much.
	weak []string
}

// note records something a step did that the script did not literally ask for.
func (r *flowRun) note(msg string) {
	line := fmt.Sprintf("step %d: %s", r.stepIdx+1, msg)
	if !slices.Contains(r.notes, line) && len(r.notes) < 5 {
		r.notes = append(r.notes, line)
	}
}

func (r *flowRun) run() (issue *ValidationIssue) {
	defer func() {
		for _, s := range r.sessions {
			for _, c := range s.closeTabs {
				c()
			}
			s.cancel()
		}
	}()
	defer func() {
		// A panic inside chromedp plumbing must not take down the whole run.
		if p := recover(); p != nil {
			issue = r.issue(len(r.flow.Steps)-1, FlowStep{}, flowFailure{"navigation", fmt.Sprintf("flow runner crashed: %v", p)}, nil, "", "")
		}
	}()
	if len(r.flow.Steps) > 0 {
		if fail := r.seedUsers(); fail != nil {
			return r.issue(0, r.flow.Steps[0], *fail, nil, "", "")
		}
	}
	for i, st := range r.flow.Steps {
		sess, err := r.session(st.session())
		if err != nil {
			r.envErr = err
			return nil
		}
		r.stepIdx = i
		r.adoptPopup(sess)
		fail := r.step(sess, st)
		hard := r.hardErrors()
		r.noteFails(i, r.takeHTTPFails(i))
		if fail == nil && len(hard) > 0 {
			fail = &flowFailure{"js_error", "the page threw while this step ran"}
		}
		if fail != nil {
			return r.issue(i, st, *fail, hard, r.currentPath(sess), r.explain(sess, st, *fail, hard, r.recentFails))
		}
	}
	// Give async work the last step kicked off (a fetch whose .then throws) the full
	// idle window the load probe uses before calling the flow clean.
	time.Sleep(store.RuntimeIdleWindow)
	if hard := r.hardErrors(); len(hard) > 0 {
		last := len(r.flow.Steps) - 1
		st := r.flow.Steps[last]
		sess := r.sessions[st.session()]
		fail := flowFailure{"js_error", "the page threw shortly after the final step"}
		r.noteFails(last, r.takeHTTPFails(last))
		return r.issue(last, st, fail, hard, r.currentPath(sess), r.explain(sess, st, fail, hard, r.recentFails))
	}
	return nil
}

// seedUsers creates the accounts the flow declares in users before it starts. A
// signedIn user's session opens with its token where the app's auth helper keeps
// it, so it starts signed in.
func (r *flowRun) seedUsers() *flowFailure {
	if len(r.flow.Users) == 0 {
		return nil
	}
	if r.hooks == nil {
		return &flowFailure{"navigation", "this flow declares users, which this replay cannot create (no project access here)"}
	}
	names := make([]string, 0, len(r.flow.Users))
	for name := range r.flow.Users {
		names = append(names, name)
	}
	slices.Sort(names)
	r.seedTokens = map[string]string{}
	for _, name := range names {
		u := r.flow.Users[name]
		v := r.vars.get(name)
		ctx, cancel := context.WithTimeout(r.browserCtx, 15*time.Second)
		tok, err := r.hooks.SeedUser(ctx, FlowSeedUser{Email: v.Email, Username: v.Username, Password: v.Password, Role: u.Role})
		cancel()
		if err != nil {
			return &flowFailure{"navigation", fmt.Sprintf("could not create the flow's user %q: %v", name, err)}
		}
		if u.SignedIn {
			r.seedTokens[name] = tok
		}
	}
	return nil
}

// recentFailSteps is how many steps back a failed request still counts as a likely
// cause of the step that fails.
const recentFailSteps = 3

// noteFails records the failed requests of step idx and forgets older ones.
func (r *flowRun) noteFails(idx int, fails []httpFail) {
	for _, f := range fails {
		f.step = idx
		r.recentFails = append(r.recentFails, f)
	}
	kept := r.recentFails[:0]
	for _, f := range r.recentFails {
		if idx-f.step < recentFailSteps {
			kept = append(kept, f)
		}
	}
	r.recentFails = kept
}

// takeHTTPFails drains every open session's error responses since the last call, and
// moves the API responses of step idx into recentCalls, forgetting older steps'.
func (r *flowRun) takeHTTPFails(idx int) []httpFail {
	var out []httpFail
	for _, name := range r.order {
		for _, f := range r.sessions[name].errs.takeHTTPFails() {
			f.session = name
			out = append(out, f)
		}
		for _, c := range r.sessions[name].errs.takeAPICalls() {
			c.session, c.step = name, idx
			r.recentCalls = append(r.recentCalls, c)
		}
	}
	kept := r.recentCalls[:0]
	for _, c := range r.recentCalls {
		if idx-c.step < recentFailSteps {
			kept = append(kept, c)
		}
	}
	r.recentCalls = kept
	return out
}

// flowPermissions are granted to every session for the project's origin (names as
// navigator.permissions knows them).
var flowPermissions = []string{"clipboard-read", "clipboard-write", "notifications", "geolocation", "camera", "microphone"}

// flowClockJS replaces the page's Date with one running offset ms ahead (behind
// when negative) of the real clock. Timers keep real time; only what the app reads
// as "now" moves, which is what day-, streak- and countdown-based UIs look at.
const flowClockJS = `(function (off) {
  var Real = window.__ofRealDate || Date;
  window.__ofRealDate = Real;
  var now = function () { return Real.now() + off; };
  function OFDate() {
    var a = Array.prototype.slice.call(arguments);
    if (!new.target) return new Real(now()).toString();
    return Reflect.construct(Real, a.length ? a : [now()], new.target);
  }
  OFDate.prototype = Real.prototype;
  OFDate.now = now; OFDate.parse = Real.parse; OFDate.UTC = Real.UTC;
  window.Date = OFDate;
})(%d)`

// flowDownloads follows the browser's downloads, so a download step can wait for
// the file its click started.
type flowDownloads struct {
	mu    sync.Mutex
	order []string           // guids in the order downloads began
	names map[string]string  // guid -> suggested file name
	state map[string]string  // guid -> inProgress | completed | canceled
	bytes map[string]float64 // guid -> bytes received
}

// flowMobileUA is sent by mobile/tablet sessions so user-agent sniffing apps take
// their phone path too.
// The trailing marker keeps these page views out of the real-traffic check that
// guards replays on published apps (headless Chrome's own agent is recognised by
// its "HeadlessChrome" token, which this override drops).
const flowMobileUA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36 " + flowsnap.FlowBrowserMarker

// flowRealErrors are load-probe noise patterns a flow does NOT ignore: a flow's
// browser has a camera and microphone, grants notifications and runs in a secure
// context, so these failures are the app's own (a wrong service worker path, a
// media call the app mishandles), not the headless environment's.
var flowRealErrors = []string{"navigator.mediadevices", "getusermedia", "notification permission", "service worker registration failed"}

// crossOriginScriptPrefix marks a soft load failure of a script from another origin.
const crossOriginScriptPrefix = "[load_failed_asset] Script ("

func (r *flowRun) currentPath(s *flowSession) string {
	if s == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	var p string
	_ = chromedp.Run(ctx, chromedp.Evaluate(`location.pathname + location.search`, &p))
	return p
}

// flowUploadTypes are the MIME types of the upload fixtures, as a browser would
// report them for a dropped file.
var flowUploadTypes = map[string]string{
	"image.png": "image/png", "photo.jpg": "image/jpeg", "notes.txt": "text/plain",
	"data.csv": "text/csv", "data.json": "application/json",
}

// flowNamedKeys maps press's key names onto chromedp's key runes.
var flowNamedKeys = map[string]string{
	"Enter": kb.Enter, "Tab": kb.Tab, "Escape": kb.Escape, "Backspace": kb.Backspace, "Delete": kb.Delete,
	"Space": " ", "ArrowUp": kb.ArrowUp, "ArrowDown": kb.ArrowDown, "ArrowLeft": kb.ArrowLeft,
	"ArrowRight": kb.ArrowRight, "Home": kb.Home, "End": kb.End, "PageUp": kb.PageUp, "PageDown": kb.PageDown,
}

// flowModifierKey is how a modifier key reaches the page: its e.key, e.code, legacy
// keyCode and the CDP modifier bit it sets while held.
type flowModifierKey struct {
	name, code string
	vk, bit    int64
}

// flowModifierKeys are pressed in this order around a chord and released in reverse.
var flowModifierKeys = []flowModifierKey{
	{"Control", "ControlLeft", 17, 2}, {"Alt", "AltLeft", 18, 1}, {"Meta", "MetaLeft", 91, 4}, {"Shift", "ShiftLeft", 16, 8},
}

// maxExplainedCalls is how many successful API responses a failure report quotes.
const maxExplainedCalls = 4

// flowRect is an element's box in viewport CSS pixels.
type flowRect struct {
	X, Y, W, H float64
}

// flowDOMResult is what flowDOMJS returns for one attempt.
type flowDOMResult struct {
	OK        bool      `json:"ok"`
	Kind      string    `json:"kind"`   // failure kind when !ok: not_found | assert
	Detail    string    `json:"detail"` // why the attempt did not succeed yet
	Rect      *flowRect `json:"rect"`   // the resolved element's box (mark ops)
	Drop      *flowRect `json:"drop"`   // the drop target's box (rects op)
	Draggable bool      `json:"draggable"`
	Shadow    bool      `json:"shadow"` // the element is inside a shadow root: address it by position
	Value     string    `json:"value"`  // what a read op found (remember); a field's current value (markfield)
	Field     string    `json:"field"`  // text | editable: how fill/type deliver the text
	Note      string    `json:"note"`   // what an op did differently from the step (a maxlength cut)
	// Filled by the context op, which describes the page when a step fails.
	URL      string   `json:"url"`
	Title    string   `json:"title"`
	Target   string   `json:"target"` // missing | hidden | disabled | visible | invalid selector
	Controls []string `json:"controls"`
	Closest  []string `json:"closest"`
	// TextMatches are visible labels that contain a missed text= selector's words
	// (or are contained in them): text= needs the WHOLE label.
	TextMatches []string `json:"textMatches"`
	// What the page shows where the step looked: the in: elements' text, where the
	// wanted text is instead (or still is), the closest text when it is nowhere, and
	// the elements a count or visibility step matched.
	ScopeState   string   `json:"scopeState"` // visible | hidden | missing | invalid selector
	Scope        []string `json:"scope"`
	ScopeCount   int      `json:"scopeCount"`
	FoundIn      []string `json:"foundIn"`
	FoundInScope bool     `json:"foundInScope"`
	HiddenIn     string   `json:"hiddenIn"`
	Near         string   `json:"near"`
	Matches      []string `json:"matches"`
	MatchCount   int      `json:"matchCount"`
	HiddenCount  int      `json:"hiddenCount"`
}

// flowDOMJS performs one attempt of a step inside the page. Selectors are CSS, or
// `text=<visible text>` which picks the innermost visible element whose folded
// text (or value / aria-label for inputs and icon buttons) equals it - see fold.
//
//go:embed js/flowdom.js
var flowDOMJS string

// maxFlowConsoleLines is how many console lines one session adds to a failure report.
const maxFlowConsoleLines = 12

// issue builds the finding for a failed step. The message names both possible
// fixes, because a red flow means the app is broken OR the script no longer matches
// the UI, and only the model knows which change was intended.
func (r *flowRun) issue(idx int, st FlowStep, fail flowFailure, hard []string, path, explanation string) *ValidationIssue {
	f := r.flow
	var b strings.Builder
	fmt.Fprintf(&b, "user flow %q (of_test.js) failed at step %d of %d %s", f.Name, idx+1, len(f.Steps), st)
	if st.As != "" {
		fmt.Fprintf(&b, " (as %s)", st.As)
	}
	fmt.Fprintf(&b, ": %s", fail.detail)
	if path != "" {
		fmt.Fprintf(&b, " (page %s)", path)
	}
	if len(hard) > 0 {
		b.WriteString(".\nJS errors while this step ran:\n  - ")
		b.WriteString(strings.Join(hard, "\n  - "))
	}
	if tail := r.consoleTail(); tail != "" {
		b.WriteString("\nPage console output before the failure (the app's own log lines; when neither they nor the API responses below show the cause, add a console.log at a suspect line and replay this flow):\n  " + tail)
	}
	if explanation != "" {
		b.WriteString("\n" + explanation)
	} else {
		b.WriteString("\nA real user doing this journey hits the same failure. If the journey is right, fix the app code. " +
			"If the UI deliberately changed (renamed button, new route), re-save of_test.js with manage_testing action=write_flows instead.")
	}

	sig := fmt.Sprintf("flow|%s|%d|%s", f.Name, idx+1, fail.kind)
	if rs := runtimeSignature(hard); rs != "" {
		sig += "|" + rs
	}
	return &ValidationIssue{
		Severity:      "error",
		Category:      "flow",
		Page:          path,
		File:          FlowPath,
		Deterministic: true,
		Signature:     sig,
		Message:       b.String(),
	}
}

// flowFixtures creates the files upload attaches, once per run, in a temp dir.
type flowFixtures struct {
	dir string
}

func (fx *flowFixtures) path(name string) (string, error) {
	if fx.dir == "" {
		d, err := os.MkdirTemp("", "of-flow-uploads-")
		if err != nil {
			return "", err
		}
		fx.dir = d
	}
	p := filepath.Join(fx.dir, name)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	var data []byte
	switch name {
	case "image.png", "photo.jpg":
		img := image.NewRGBA(image.Rect(0, 0, 64, 64))
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 4), 160, 255})
			}
		}
		var buf bytes.Buffer
		var err error
		if name == "image.png" {
			err = png.Encode(&buf, img)
		} else {
			err = jpeg.Encode(&buf, img, nil)
		}
		if err != nil {
			return "", err
		}
		data = buf.Bytes()
	case "notes.txt":
		data = []byte("Hello from of_test.js.\nA second line.\n")
	case "data.csv":
		data = []byte("name,value\nalpha,1\nbeta,2\ngamma,3\n")
	case "data.json":
		data = []byte(`{"name":"alpha","items":[1,2,3]}` + "\n")
	default:
		return "", fmt.Errorf("unknown fixture %q", name)
	}
	return p, os.WriteFile(p, data, 0o600)
}

// downloadDir is where the run's sessions save what they download.
func (fx *flowFixtures) downloadDir() (string, error) {
	if fx.dir == "" {
		d, err := os.MkdirTemp("", "of-flow-uploads-")
		if err != nil {
			return "", err
		}
		fx.dir = d
	}
	dir := filepath.Join(fx.dir, "downloads")
	return dir, os.MkdirAll(dir, 0o700)
}

func (fx *flowFixtures) cleanup() {
	if fx.dir != "" {
		_ = os.RemoveAll(fx.dir)
	}
}

var flowSafeIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CleanupFlowUsers deletes the accounts flow runs registered: the given identities'
// emails and usernames, plus any earlier run's leftovers (an of-test+…@example.test
// address no real user has). Best effort: rows another table still references stay.
func CleanupFlowUsers(db dbh.Handle, accounts []FlowVars) {
	if db == nil {
		return
	}
	rows, err := db.Query(`SELECT table_name, username_column FROM of_auth_endpoints`)
	if err != nil {
		return
	}
	type target struct{ table, col string }
	var targets []target
	for rows.Next() {
		var t target
		if rows.Scan(&t.table, &t.col) == nil && flowSafeIdent.MatchString(t.table) && flowSafeIdent.MatchString(t.col) {
			targets = append(targets, t)
		}
	}
	// Best effort: a partial target list still cleans what it found.
	_ = rows.Err()
	rows.Close()
	var ids []any
	for _, a := range accounts {
		ids = append(ids, a.Email, a.Username)
	}
	for _, t := range targets {
		col := security.QuoteIdent(t.col)
		q := fmt.Sprintf(`DELETE FROM %s WHERE %s LIKE ?`, security.QuoteIdent(t.table), col)
		args := []any{"of-test+%@" + flowTestEmailDomain}
		if len(ids) > 0 {
			q += fmt.Sprintf(` OR %s IN (?%s)`, col, strings.Repeat(", ?", len(ids)-1))
			args = append(args, ids...)
		}
		_, _ = db.Exec(q, args...)
	}
}
