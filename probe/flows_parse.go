package probe

// of_test.js: the per-project user-journey script the BUILD stage writes and the
// flow_tests validator runs. The file is a real ES module whose default export is
// DATA (flows of declarative steps), not code: a page navigation wipes in-page JS
// state, so the steps are driven from Go over CDP and the script only says what to do.
// Parsing never touches a browser: esbuild lowers the module to CommonJS and goja
// evaluates it to JSON, so a syntax error or unknown verb is rejected at save time.
//
// The verb set is deliberately generic so one format covers every kind of project
// with a UI: DOM apps and sites assert on text, URLs and elements; canvas/WebGL
// games, maps and drawing tools drive the pointer and keyboard and assert on pixels
// changing or the render loop running; anything else asserts on app state with a JS
// expression; multi-user apps act as several isolated users with `as`.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/evanw/esbuild/pkg/api"

	"openfabrica/store"
)

// FlowKind and FlowPath locate the script in of_files. A dedicated kind keeps it out
// of the public server (which serves only kind='file' assets) and out of the page
// bundle: it describes the app, it is not part of it.
const (
	FlowKind = "test"
	FlowPath = store.FlowScriptPath
)

// Bounds that keep one run inside a validator's budget.
const (
	maxFlows            = 25
	maxStepsPerFlow     = 80
	maxSessionsPerFlow  = 4
	defaultStepTimeout  = 5 * time.Second
	maxStepTimeout      = 60 * time.Second
	flowParseEvalBudget = 2 * time.Second
	maxFlowWait         = 10 * time.Second
	maxKeyHold          = 5 * time.Second
	maxDragPoints       = 50
	defaultSession      = "main"
)

// FlowSuite is the parsed default export of of_test.js.
type FlowSuite struct {
	Flows []Flow `json:"flows"`
	// Notes lists the slips the parser rewrote to canonical steps (see
	// flows_normalize.go), so the author learns the format without a failed save.
	Notes []string `json:"-"`
}

// Flow is one named user journey. Every session it uses (the default "main" plus any
// step's `as`) is a fresh, isolated, logged-out browser profile.
type Flow struct {
	Name     string     `json:"name"`
	Viewport string     `json:"viewport,omitempty"` // desktop (default) | tablet | mobile (| -landscape)
	Steps    []FlowStep `json:"steps"`
	// Users are accounts created for this flow before it starts, keyed by session
	// ("main", "admin", ...): an admin or another role a signup cannot produce, or
	// a user already signed in for apps whose only login is an outside provider.
	// Their {{email}}/{{username}}/{{password}} are that session's variables.
	Users map[string]FlowUser `json:"users,omitempty"`
}

// FlowUser is an account a flow starts with.
type FlowUser struct {
	Role     string `json:"role,omitempty"`     // the auth endpoint's default role when empty
	SignedIn bool   `json:"signedIn,omitempty"` // the session starts signed in as this user
}

// FlowStep is one action or assertion: exactly one verb field is set, and the
// qualifier fields (value, count, to, path, at, hold) refine it where that verb
// allows. As and Timeout apply to every verb.
type FlowStep struct {
	// Navigation and pointer.
	Goto   string `json:"goto,omitempty"`
	Reload bool   `json:"reload,omitempty"` // reload the current page, as the browser's reload button does
	// Leave closes the session's tab. A later goto as the same session opens a new browser.
	Leave    bool   `json:"leave,omitempty"`
	Click    string `json:"click,omitempty"`
	DblClick string `json:"dblclick,omitempty"`
	Hover    string `json:"hover,omitempty"`
	Drag     string `json:"drag,omitempty"`
	Scroll   string `json:"scroll,omitempty"`
	// Forms, keyboard and files.
	Fill    string `json:"fill,omitempty"`
	Type    string `json:"type,omitempty"` // like fill, but key by key: for inputs that react to each keystroke
	Select  string `json:"select,omitempty"`
	Check   string `json:"check,omitempty"`
	Uncheck string `json:"uncheck,omitempty"`
	Press   string `json:"press,omitempty"`
	Upload  string `json:"upload,omitempty"`
	// Download clicks the element and waits for the file it downloads to arrive.
	Download string `json:"download,omitempty"`
	// Browser state: go offline and back, answer the next alert/confirm/prompt, and
	// move the page's clock (Date) for apps that change with the day.
	Offline *bool  `json:"offline,omitempty"`
	Dialog  string `json:"dialog,omitempty"`
	Clock   string `json:"clock,omitempty"`
	// Beyond the page: an HTTP request (API journeys, and setting up or checking
	// data), what the app sent (email - kept, not delivered, during a replay),
	// and the project's scheduled tasks and inbound webhooks.
	Request        string `json:"request,omitempty"` // "POST /api/orders"
	ExpectResponse any    `json:"expectResponse,omitempty"`
	ExpectEmail    string `json:"expectEmail,omitempty"`
	RunSchedule    string `json:"runSchedule,omitempty"`
	Webhook        string `json:"webhook,omitempty"`
	// Values carried between steps: the text, value or attribute of an element,
	// usable later as {{name}} (a generated share link, room code, order number).
	Remember string `json:"remember,omitempty"`
	// Time.
	Wait    int    `json:"wait,omitempty"`
	WaitFor string `json:"waitFor,omitempty"`
	// Assertions.
	ExpectText      string `json:"expectText,omitempty"`
	ExpectNoText    string `json:"expectNoText,omitempty"`
	ExpectURL       string `json:"expectUrl,omitempty"`
	ExpectVisible   string `json:"expectVisible,omitempty"`
	ExpectHidden    string `json:"expectHidden,omitempty"`
	ExpectCount     string `json:"expectCount,omitempty"`
	ExpectJS        string `json:"expectJs,omitempty"`
	ExpectAnimating bool   `json:"expectAnimating,omitempty"`
	Snapshot        string `json:"snapshot,omitempty"`
	ExpectChanged   string `json:"expectChanged,omitempty"`

	// Qualifiers.
	Value   *string           `json:"value,omitempty"`
	Count   *int              `json:"count,omitempty"`
	To      string            `json:"to,omitempty"`
	Path    [][2]float64      `json:"path,omitempty"`
	At      *[2]float64       `json:"at,omitempty"`
	Hold    int               `json:"hold,omitempty"`
	From    string            `json:"from,omitempty"`
	In      string            `json:"in,omitempty"`      // scope a step to the elements matching this selector
	Body    any               `json:"body,omitempty"`    // request / webhook body: an object (sent as JSON) or a string
	Headers map[string]string `json:"headers,omitempty"` // request headers
	Status  *int              `json:"status,omitempty"`  // the status a request must answer with (default: any 2xx)
	Attr    string            `json:"attr,omitempty"`
	As      string            `json:"as,omitempty"`
	Timeout int               `json:"timeout,omitempty"` // milliseconds
}

// verb returns the step's single verb and its argument ("" when none is set; the
// parser rejects that and multi-verb steps before a run ever sees them).
func (s FlowStep) verb() (string, string) {
	for _, v := range s.verbs() {
		if v[1] != "" {
			return v[0], v[1]
		}
	}
	return "", ""
}

func (s FlowStep) verbs() [][2]string {
	animating := ""
	if s.ExpectAnimating {
		animating = "true"
	}
	reload := ""
	if s.Reload {
		reload = "true"
	}
	leave := ""
	if s.Leave {
		leave = "true"
	}
	wait := ""
	if s.Wait != 0 {
		wait = strconv.Itoa(s.Wait)
	}
	offline := ""
	if s.Offline != nil {
		offline = strconv.FormatBool(*s.Offline)
	}
	response := ""
	if s.ExpectResponse != nil {
		b, _ := json.Marshal(s.ExpectResponse)
		response = string(b)
	}
	return [][2]string{
		{"goto", s.Goto}, {"reload", reload}, {"leave", leave}, {"click", s.Click}, {"dblclick", s.DblClick}, {"hover", s.Hover},
		{"drag", s.Drag}, {"scroll", s.Scroll}, {"fill", s.Fill}, {"type", s.Type}, {"select", s.Select},
		{"check", s.Check}, {"uncheck", s.Uncheck}, {"press", s.Press}, {"upload", s.Upload}, {"download", s.Download},
		{"offline", offline}, {"dialog", s.Dialog}, {"clock", s.Clock}, {"remember", s.Remember},
		{"request", s.Request}, {"expectResponse", response}, {"expectEmail", s.ExpectEmail},
		{"runSchedule", s.RunSchedule}, {"webhook", s.Webhook},
		{"wait", wait}, {"waitFor", s.WaitFor}, {"expectText", s.ExpectText},
		{"expectNoText", s.ExpectNoText}, {"expectUrl", s.ExpectURL}, {"expectVisible", s.ExpectVisible},
		{"expectHidden", s.ExpectHidden}, {"expectCount", s.ExpectCount}, {"expectJs", s.ExpectJS},
		{"expectAnimating", animating}, {"snapshot", s.Snapshot}, {"expectChanged", s.ExpectChanged},
	}
}

// qualifierVerbs lists, per qualifier, the verbs it may refine.
var qualifierVerbs = map[string]map[string]bool{
	"value": {"fill": true, "type": true, "select": true, "upload": true, "download": true, "dialog": true},
	"count": {"expectCount": true},
	"to":    {"drag": true, "expectEmail": true},
	"path":  {"drag": true},
	"at":    {"click": true, "dblclick": true, "hover": true},
	"hold":  {"press": true},
	"from":  {"remember": true},
	// in scopes a step to the elements matching it (a row, card or column), so a
	// flow can act on "the Delete button of MY record" or assert inside one place.
	"in": {"expectText": true, "expectNoText": true, "click": true, "dblclick": true, "hover": true,
		"fill": true, "type": true, "select": true, "check": true, "uncheck": true,
		"waitFor": true, "expectVisible": true, "expectHidden": true, "expectCount": true, "download": true},
	"attr":    {"remember": true},
	"body":    {"request": true, "webhook": true},
	"headers": {"request": true},
	"status":  {"request": true, "webhook": true},
}

func (s FlowStep) qualifiers() []string {
	var q []string
	if s.Value != nil {
		q = append(q, "value")
	}
	if s.Count != nil {
		q = append(q, "count")
	}
	if s.To != "" {
		q = append(q, "to")
	}
	if s.Path != nil {
		q = append(q, "path")
	}
	if s.At != nil {
		q = append(q, "at")
	}
	if s.Hold != 0 {
		q = append(q, "hold")
	}
	if s.From != "" {
		q = append(q, "from")
	}
	if s.In != "" {
		q = append(q, "in")
	}
	if s.Attr != "" {
		q = append(q, "attr")
	}
	if s.Body != nil {
		q = append(q, "body")
	}
	if s.Headers != nil {
		q = append(q, "headers")
	}
	if s.Status != nil {
		q = append(q, "status")
	}
	return q
}

// flowClockRe matches a relative clock move: +1d, -2h, +30m, +90s, +1w.
var flowClockRe = regexp.MustCompile(`^([+-])(\d{1,5})([smhdw])$`)

// parseFlowClock reads a clock step: a move relative to the page's current time
// ("+1d", "-2h") or an absolute local date or time ("2026-03-02", "2026-03-02T09:00").
func parseFlowClock(v string) (rel time.Duration, abs time.Time, err error) {
	v = strings.TrimSpace(v)
	if m := flowClockRe.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[2])
		unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[3]]
		rel = time.Duration(n) * unit
		if m[1] == "-" {
			rel = -rel
		}
		return rel, time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, perr := time.ParseInLocation(layout, v, time.Local); perr == nil {
			return 0, t, nil
		}
	}
	return 0, time.Time{}, fmt.Errorf(`clock takes a move ("+1d", "-2h", "+30m", "+1w") or a local date/time ("2026-03-02", "2026-03-02T09:00"), not %q`, v)
}

func (s FlowStep) session() string {
	if s.As == "" {
		return defaultSession
	}
	return s.As
}

func (s FlowStep) timeout() time.Duration {
	if s.Timeout <= 0 {
		// Work that happens off the page takes longer than a render: a mail goes
		// out through the job queue (polled every few seconds), a scheduled task or
		// an API call runs server-side.
		switch v, _ := s.verb(); v {
		case "expectEmail":
			return 20 * time.Second
		case "runSchedule":
			return 45 * time.Second
		case "request", "webhook":
			return 15 * time.Second
		}
		return defaultStepTimeout
	}
	return time.Duration(s.Timeout) * time.Millisecond
}

// String renders the step compactly for failure messages: {"click":"text=Save"}.
func (s FlowStep) String() string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{?}"
	}
	return string(b)
}

// flowKeys are the named keys press accepts, beyond a single printable character.
// The modifiers count as keys too, so a game that sprints while Shift is held can
// {press: "Shift", hold: 800} or hold it around a key with {press: "Shift+w", hold: 800}.
var flowKeys = map[string]bool{
	"Enter": true, "Tab": true, "Escape": true, "Backspace": true, "Delete": true, "Space": true,
	"ArrowUp": true, "ArrowDown": true, "ArrowLeft": true, "ArrowRight": true,
	"Home": true, "End": true, "PageUp": true, "PageDown": true,
	"Shift": true, "Control": true, "Alt": true, "Meta": true,
}

// flowModifiers maps the modifier names a press chord accepts ("Shift+Enter",
// "Ctrl+s", "Cmd+k") onto the CDP modifier bits: Alt=1, Control=2, Meta=4, Shift=8.
var flowModifiers = map[string]int64{
	"Alt": 1, "Option": 1,
	"Control": 2, "Ctrl": 2,
	"Meta": 4, "Cmd": 4, "Command": 4,
	"Shift": 8,
}

// parsePressChord splits a press argument into its modifier bits and the key: one
// character or a named key, optionally preceded by modifiers joined with "+"
// ("Shift+Enter", "Control+Shift+z"). A lone "+" is the plus key, not a chord.
func parsePressChord(arg string) (mods int64, key string, err error) {
	parts := []string{arg}
	if len([]rune(arg)) > 1 && strings.Contains(arg, "+") {
		parts = strings.Split(arg, "+")
		if strings.HasSuffix(arg, "++") { // "Shift++": the plus key itself
			parts = append(parts[:len(parts)-2], "+")
		}
	}
	key = parts[len(parts)-1]
	for _, m := range parts[:len(parts)-1] {
		bit, ok := flowModifiers[m]
		if !ok {
			return 0, "", fmt.Errorf("press %q: %q is not a modifier (Shift, Control, Alt, Meta)", arg, m)
		}
		mods |= bit
	}
	if len([]rune(key)) != 1 && !flowKeys[key] {
		return 0, "", fmt.Errorf("press takes one character or a key name (Enter, Tab, Escape, Space, Backspace, Delete, Arrow*, Home, End, PageUp, PageDown, Shift, Control, Alt, Meta), optionally with modifiers (\"Shift+Enter\", \"Control+a\"; with hold, \"Shift+w\" holds both), not %q", arg)
	}
	return mods, key, nil
}

// flowUploads are the fixture files upload can attach (generated per run).
var flowUploads = map[string]bool{
	"image.png": true, "photo.jpg": true, "notes.txt": true, "data.csv": true, "data.json": true,
}

var (
	// flowVarRe matches {{variable}} or {{variable:session}}: a step's variables
	// resolve against its own session unless another one is named.
	flowVarRe      = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*(?::\s*([A-Za-z][A-Za-z0-9_]*)\s*)?\}\}`)
	flowRememberRe = regexp.MustCompile(`^[a-z][A-Za-z0-9_]{0,30}$`)
	flowAttrRe     = regexp.MustCompile(`^[a-zA-Z_:][-a-zA-Z0-9_:.]{0,40}$`)
	flowVars       = map[string]bool{"email": true, "password": true, "username": true, "run": true, "phone": true, "totp": true}
	flowRoleRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{0,40}$`)
	flowWebhookRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	flowHeaderRe   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	flowRequestRe  = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(\S+)$`)
	// flowSpecialFromRe matches remember sources that are not page elements: the
	// session's last response (a JSON path into it), or its last matched message.
	flowSpecialFromRe = regexp.MustCompile(`^(response(\.[A-Za-z0-9_$-]+)*|email\.(link|code|body|subject))$`)
	flowSessionRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,19}$`)
	flowViewports     = map[string]bool{"": true, "desktop": true, "tablet": true, "mobile": true, "mobile-landscape": true, "tablet-landscape": true}
)

// ParseFlowSource evaluates an of_test.js module and validates the flows it exports.
// Errors are phrased for the model that wrote the file: each names what to change.
func ParseFlowSource(src string) (*FlowSuite, error) {
	if strings.TrimSpace(src) == "" {
		return nil, errors.New("of_test.js is empty; export default { flows: [...] }")
	}
	res := api.Transform(src, api.TransformOptions{Loader: api.LoaderJS, Format: api.FormatCommonJS})
	if len(res.Errors) > 0 {
		m := res.Errors[0]
		if m.Location != nil {
			return nil, fmt.Errorf("of_test.js does not parse: %s (%d:%d)%s", m.Text, m.Location.Line, m.Location.Column, flowDSLRef())
		}
		return nil, fmt.Errorf("of_test.js does not parse: %s%s", m.Text, flowDSLRef())
	}

	vm := goja.New()
	vm.SetMaxCallStackSize(500)
	timer := time.AfterFunc(flowParseEvalBudget, func() { vm.Interrupt("of_test.js took too long to evaluate") })
	defer timer.Stop()
	script := "var module = { exports: {} }; var exports = module.exports;\n" + string(res.Code) +
		"\n;JSON.stringify((module.exports && module.exports.default) || module.exports)"
	v, err := vm.RunString(script)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "require is not defined") {
			return nil, errors.New("of_test.js must not import anything: it is plain data, export default { flows: [...] }")
		}
		return nil, fmt.Errorf("of_test.js threw while evaluating: %s", msg)
	}
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, errors.New("of_test.js has no default export; export default { flows: [...] }")
	}

	normalized, notes := normalizeFlowJSON([]byte(v.String()))
	dec := json.NewDecoder(bytes.NewReader(normalized))
	dec.DisallowUnknownFields()
	var suite FlowSuite
	if err := dec.Decode(&suite); err != nil {
		return nil, fmt.Errorf("of_test.js default export is not { flows: [{ name, viewport?, steps: [...] }] }: %s%s (step verbs: %s; qualifiers: %s)", err, flowDecodeHint(err), strings.Join(verbNames(), ", "), flowQualifierList)
	}
	if err := suite.validate(); err != nil {
		return nil, fmt.Errorf("%w%s", err, flowDSLRef())
	}
	suite.Notes = notes
	return &suite, nil
}

func verbNames() []string {
	var out []string
	for _, v := range (FlowStep{}).verbs() {
		out = append(out, v[0])
	}
	return out
}

// flowDecodeHint adds a targeted note for the type mismatch models hit most: the
// wait (milliseconds, a number) / waitFor (an element selector, a string) pair,
// whose names are close enough that one gets written for the other. The normalizer
// rewrites the clear cases before this; this covers what is left (a boolean in
// waitFor, a non-selector string in wait), where the fix is still to swap verbs.
// flowQualifierList names the step qualifiers in one place, so the decode error and
// the DSL reference footer below cannot drift apart.
const flowQualifierList = "value, count, to, path, at, hold, from, attr, in, body, headers, status, as, timeout"

// flowDSLRef is a compact, parser-derived reference appended to every parse or
// validation failure of of_test.js. A save written without first recalling the
// user_flows guide can then be corrected in one pass instead of one round-trip per
// mistake. The verb list is built from verbNames() so it can never drift from the
// parser. This is a tool rejection message, not prompt text, so it is not a second
// home for the DSL (it points at the guide for the full reference).
func flowDSLRef() string {
	return "\n\nof_test.js is plain data: export default { flows: [{ name, viewport?, steps: [...] }] }, one verb per step." +
		"\nStep verbs: " + strings.Join(verbNames(), ", ") + "." +
		"\nQualifiers: " + flowQualifierList + "." +
		"\nSelectors: \"#id\", \"text=<exact visible label>\", or CSS (\".card:has-text('Lena')\"); give every targeted control an id." +
		"\nTokens: {{email}} {{password}} {{username}} {{phone}} {{run}} {{totp:<remembered secret>}} and names set by an earlier remember step." +
		"\nFull reference: recall the user_flows guide, section writing-flows."
}

func flowDecodeHint(err error) string {
	var ute *json.UnmarshalTypeError
	if !errors.As(err, &ute) {
		return ""
	}
	field := ute.Field
	if i := strings.LastIndex(field, "."); i >= 0 {
		field = field[i+1:]
	}
	switch field {
	case "waitFor":
		return `. waitFor takes an element selector (a string, like {waitFor: "#done"}); for a pause of N milliseconds use the wait verb: {wait: 500}`
	case "wait":
		return `. wait takes a number of milliseconds (like {wait: 500}); to wait for an element to appear use waitFor with a selector: {waitFor: "#done"}`
	}
	return ""
}

// maxValidateErrors bounds the aggregated error list so one badly-formed script cannot
// return an unbounded message.
const maxValidateErrors = 20

func (s *FlowSuite) validate() error {
	if len(s.Flows) == 0 {
		return errors.New("of_test.js exports no flows; export default { flows: [{ name, steps: [...] }] }")
	}
	if len(s.Flows) > maxFlows {
		return fmt.Errorf("of_test.js has %d flows (max %d); keep one flow per primary user journey", len(s.Flows), maxFlows)
	}
	// Collect every problem in one pass instead of stopping at the first, so a script
	// written with the wrong mental model is corrected in one re-save, not one
	// round-trip per mistake. The per-step state machine below keeps running on
	// best-effort state (remembered names, opened tabs) so later steps are judged
	// against reality, not a cascade from one earlier slip.
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	seen := map[string]bool{}
	// Remembered values carry across flows in file order, like the run variables.
	remembered := map[string]bool{}
	for i, f := range s.Flows {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			add("flow %d has no name", i+1)
			name = fmt.Sprintf("#%d", i+1)
		}
		if seen[name] {
			add("flow name %q is used twice; names must be unique", name)
		}
		seen[name] = true
		if !flowViewports[f.Viewport] {
			add("flow %q viewport must be desktop, tablet, mobile, mobile-landscape or tablet-landscape, not %q", name, f.Viewport)
		}
		if len(f.Steps) == 0 {
			add("flow %q has no steps", name)
			continue
		}
		if len(f.Steps) > maxStepsPerFlow {
			add("flow %q has %d steps (max %d); split it into smaller flows", name, len(f.Steps), maxStepsPerFlow)
		}
		for session, u := range f.Users {
			if !flowSessionRe.MatchString(session) {
				add("flow %q users: %q is not a session name (short, lowercase, like \"admin\" or \"main\")", name, session)
			}
			if !flowRoleRe.MatchString(u.Role) {
				add("flow %q users.%s: role must be a role name such as \"admin\", not %q", name, session, u.Role)
			}
		}
		requested := map[string]bool{} // sessions that made a request (for expectResponse)
		sessions := map[string]bool{}
		opened := map[string]bool{}
		snapped := map[string]bool{} // session + "\x00" + selector
		tooManySessions := false
		for j, st := range f.Steps {
			if err := st.validate(); err != nil {
				add("flow %q step %d %s: %v", name, j+1, st, err)
			}
			if err := checkFlowVars(st, remembered); err != nil {
				add("flow %q step %d %s: %v", name, j+1, st, err)
			}
			if st.Remember != "" {
				remembered[st.Remember] = true
			}
			sessions[st.session()] = true
			if len(sessions) > maxSessionsPerFlow && !tooManySessions {
				add("flow %q uses more than %d sessions (as)", name, maxSessionsPerFlow)
				tooManySessions = true
			}
			verb, arg := st.verb()
			key := st.session() + "\x00" + arg
			switch verb {
			case "snapshot":
				snapped[key] = true
			case "expectChanged":
				if !snapped[key] {
					add("flow %q step %d %s: expectChanged needs an earlier {snapshot: %q} in the same session to compare against", name, j+1, st, arg)
				}
			}
			// Each session is its own browser tab that starts on a blank page, and a
			// step without as runs in "main". A session that acts before its own goto
			// fails on the blank page every time - usually as:"x" on the opening goto
			// only, which opens a second user while the rest of the flow runs as main.
			switch verb {
			case "request", "webhook":
				requested[st.session()] = true
			case "expectResponse":
				if !requested[st.session()] {
					add("flow %q step %d %s: expectResponse checks the response to an earlier request (or webhook) step in the same session, and there is none", name, j+1, st)
				}
			case "remember":
				if strings.HasPrefix(st.From, "response") && !requested[st.session()] {
					add("flow %q step %d %s: from %q reads the response to an earlier request step in the same session, and there is none", name, j+1, st, st.From)
				}
			}
			if verb == "leave" {
				if !opened[st.session()] {
					add("flow %q step %d %s: %q has no open page to leave; leave closes a session that already did a goto", name, j+1, st, st.session())
				}
				// The tab is gone: a later step as this session must open it again with a goto.
				opened[st.session()] = false
				delete(requested, st.session())
				continue
			}
			switch verb {
			case "goto", "request", "webhook":
				// A request opens the site's root in its tab first.
				opened[st.session()] = true
			case "wait", "dialog", "clock", "offline", "expectResponse", "expectEmail", "runSchedule":
				// They need no page yet (a request opens the site itself).
			case "remember":
				if flowSpecialFromRe.MatchString(st.From) {
					break
				}
				fallthrough
			default:
				if !opened[st.session()] {
					add("flow %q step %d %s runs as %q, which has not opened a page yet - every session (as:) is its own tab starting blank, and steps without as run as \"main\". Start each session with its own goto, or put as on every step that user takes", name, j+1, st, st.session())
				}
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	more := ""
	if len(errs) > maxValidateErrors {
		more = fmt.Sprintf("\n...and %d more problem(s); fix these and re-save.", len(errs)-maxValidateErrors)
		errs = errs[:maxValidateErrors]
	}
	return errors.New(strings.Join(errs, "\n") + more)
}

func (s FlowStep) validate() error {
	n := 0
	for _, v := range s.verbs() {
		if v[1] != "" {
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("needs exactly one of: %s", strings.Join(verbNames(), ", "))
	}
	if n > 1 {
		return errors.New("has more than one verb; split it into one step per action")
	}
	verb, arg := s.verb()
	for _, q := range s.qualifiers() {
		if !qualifierVerbs[q][verb] {
			return fmt.Errorf("%s does not apply to %s", q, verb)
		}
	}
	switch verb {
	case "goto":
		if !strings.HasPrefix(arg, "/") && !strings.HasPrefix(arg, "{{") {
			return errors.New(`goto takes a site-relative path such as "/login", or a remembered link such as "{{shareLink}}"`)
		}
	case "remember":
		if !flowRememberRe.MatchString(arg) {
			return fmt.Errorf("remember takes a short name like \"shareLink\" (letters, digits, _), not %q", arg)
		}
		if flowVars[arg] {
			return fmt.Errorf("%q is a built-in variable; remember it under another name", arg)
		}
		if s.From == "" {
			return errors.New(`remember needs from: the element to read, e.g. { remember: "shareLink", from: "#link", attr: "href" }`)
		}
		if s.Attr != "" && !flowAttrRe.MatchString(s.Attr) {
			return fmt.Errorf("attr must be an attribute name such as \"href\", not %q", s.Attr)
		}
		if s.Attr != "" && flowSpecialFromRe.MatchString(s.From) {
			return fmt.Errorf("attr reads an element's attribute; from %q is not an element", s.From)
		}
	case "fill", "type", "select":
		if s.Value == nil {
			return fmt.Errorf("%s needs a value", verb)
		}
	case "upload":
		if s.Value == nil || !flowUploads[*s.Value] {
			return errors.New("upload needs value: one of image.png, photo.jpg, notes.txt, data.csv, data.json")
		}
	case "expectCount":
		if s.Count == nil || *s.Count < 0 {
			return errors.New("expectCount needs a non-negative count")
		}
	case "press":
		if _, _, err := parsePressChord(arg); err != nil {
			return err
		}
		if s.Hold < 0 || time.Duration(s.Hold)*time.Millisecond > maxKeyHold {
			return fmt.Errorf("hold must be between 0 and %d ms", maxKeyHold.Milliseconds())
		}
	case "wait":
		if s.Wait < 0 || time.Duration(s.Wait)*time.Millisecond > maxFlowWait {
			return fmt.Errorf("wait must be between 1 and %d ms", maxFlowWait.Milliseconds())
		}
	case "drag":
		if (s.To == "") == (s.Path == nil) {
			return errors.New(`drag needs exactly one of to (a drop-target selector) or path ([[x,y], ...] points inside the dragged element, 0..1)`)
		}
		if s.Path != nil {
			if len(s.Path) < 2 || len(s.Path) > maxDragPoints {
				return fmt.Errorf("drag path needs 2 to %d points", maxDragPoints)
			}
			for _, p := range s.Path {
				if !fraction(p[0]) || !fraction(p[1]) {
					return errors.New("drag path points are fractions of the element's box: [x, y] with 0 <= x, y <= 1")
				}
			}
		}
	case "request":
		m := flowRequestRe.FindStringSubmatch(arg)
		if m == nil || (!strings.HasPrefix(m[2], "/") && !strings.HasPrefix(m[2], "{{")) {
			return fmt.Errorf(`request takes a method and a site path: "POST /api/orders", "GET /api/orders/{{orderId}}", not %q`, arg)
		}
	case "webhook":
		if !flowWebhookRe.MatchString(arg) {
			return fmt.Errorf("webhook takes the name of one of the project's inbound webhooks (POST /webhooks/<name>), not %q", arg)
		}
	case "runSchedule":
		if strings.TrimSpace(arg) == "" {
			return errors.New("runSchedule takes the name of one of the project's scheduled tasks")
		}
	case "dialog":
		if arg != "accept" && arg != "dismiss" {
			return fmt.Errorf(`dialog takes "accept" or "dismiss" (how to answer the next alert, confirm or prompt; value is the prompt's answer), not %q`, arg)
		}
	case "clock":
		if _, _, err := parseFlowClock(arg); err != nil {
			return err
		}
	case "expectJs":
		if strings.Contains(arg, ":has-text(") {
			return errors.New(`expectJs runs in the page, where :has-text is not valid CSS (querySelector throws on it); check the element with a step instead: {expectVisible: ".card:has-text('Lena')"}, or {expectText: "...", in: ".card:has-text('Lena')"}`)
		}
		if res := api.Transform("("+arg+")", api.TransformOptions{Loader: api.LoaderJS}); len(res.Errors) > 0 {
			return fmt.Errorf("expectJs must be one JS expression (%s); to use statements, wrap them and return the check: \"(() => { const m = document.querySelector('#x'); return !!m && m.textContent.length > 0; })()\"", res.Errors[0].Text)
		}
	}
	from := s.From
	if flowSpecialFromRe.MatchString(from) {
		from = ""
	}
	to := s.To
	if verb != "drag" {
		to = ""
	}
	for _, sel := range []string{elementSelector(verb, arg), s.In, to, from} {
		if err := checkHasText(sel); err != nil {
			return err
		}
	}
	if s.At != nil && (!fraction(s.At[0]) || !fraction(s.At[1])) {
		return errors.New("at is a point inside the element as fractions of its box: [x, y] with 0 <= x, y <= 1")
	}
	for k := range s.Headers {
		if !flowHeaderRe.MatchString(k) {
			return fmt.Errorf("headers: %q is not a header name", k)
		}
	}
	if s.Status != nil && (*s.Status < 100 || *s.Status > 599) {
		return fmt.Errorf("status must be an HTTP status code (100-599), not %d", *s.Status)
	}
	if s.As != "" && !flowSessionRe.MatchString(s.As) {
		return fmt.Errorf("as must be a short lowercase session name like \"bob\", not %q", s.As)
	}
	if s.Timeout < 0 || time.Duration(s.Timeout)*time.Millisecond > maxStepTimeout {
		return fmt.Errorf("timeout must be between 0 and %d ms", maxStepTimeout.Milliseconds())
	}
	return nil
}

// checkFlowVars rejects a {{variable}} that is neither built in nor remembered by an
// earlier step, and a :session qualifier on a remembered value (those are shared by
// every session, which is the point: one user's link, opened by another).
// rememberedNeeds lists the remembered values a flow reads that an earlier flow must
// have captured (names it reads before remembering them itself), in first-use order.
func (f Flow) rememberedNeeds() []string {
	own := map[string]bool{}
	seen := map[string]bool{}
	var out []string
	for _, st := range f.Steps {
		for _, str := range st.varStrings() {
			for _, m := range flowVarRe.FindAllStringSubmatch(str, -1) {
				name := m[1]
				if name == "totp" && m[2] != "" {
					name = m[2] // {{totp:secret}} reads the remembered secret
				}
				if flowVars[name] || own[name] || seen[name] {
					continue
				}
				seen[name] = true
				out = append(out, name)
			}
		}
		if st.Remember != "" {
			own[st.Remember] = true
		}
	}
	return out
}

// remembers lists the names a flow's remember steps capture.
func (f Flow) remembers() []string {
	var out []string
	for _, st := range f.Steps {
		if st.Remember != "" {
			out = append(out, st.Remember)
		}
	}
	return out
}

// varStrings are the step's strings that may hold {{variables}}: its argument,
// qualifiers, headers, and every string in a request body.
func (s FlowStep) varStrings() []string {
	_, arg := s.verb()
	out := []string{arg, derefStr(s.Value), s.To, s.From, s.In}
	for _, v := range s.Headers {
		out = append(out, v)
	}
	if s.Body != nil {
		b, _ := json.Marshal(s.Body)
		out = append(out, string(b))
	}
	return out
}

func checkFlowVars(st FlowStep, remembered map[string]bool) error {
	for _, str := range st.varStrings() {
		for _, m := range flowVarRe.FindAllStringSubmatch(str, -1) {
			switch {
			case m[1] == "totp":
				if !remembered[m[2]] {
					return fmt.Errorf("{{totp:%s}} computes a 2FA code from the secret an earlier remember step captured as %q, and none did (remember the secret the app shows during 2FA setup first)", m[2], m[2])
				}
			case flowVars[m[1]]:
			case remembered[m[1]] && m[2] != "":
				return fmt.Errorf("{{%s:%s}}: remembered values are shared by all sessions, write {{%s}}", m[1], m[2], m[1])
			case remembered[m[1]]:
			default:
				return fmt.Errorf("unknown variable {{%s}}; available: {{email}}, {{password}}, {{username}}, {{phone}}, {{run}} (optionally {{email:<session>}}), {{totp:<remembered secret>}} and names set by an earlier remember step", m[1])
			}
		}
	}
	return nil
}

func fraction(f float64) bool { return f >= 0 && f <= 1 }

// elementSelector returns arg when verb takes an element selector, "" otherwise
// (goto's path, press's key, expectText's text, ...).
func elementSelector(verb, arg string) string {
	switch verb {
	case "click", "dblclick", "hover", "drag", "scroll", "fill", "type", "select", "check", "uncheck", "upload", "download",
		"waitFor", "expectVisible", "expectHidden", "expectCount", "snapshot", "expectChanged":
		return arg
	}
	return ""
}

// hasTextRe matches one :has-text("...") or :has-text('...') and what follows it.
var hasTextRe = regexp.MustCompile(`:has-text\(\s*(?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')\s*\)`)

// checkHasText validates :has-text("...") in a CSS selector: the runner supports it
// (the element's visible text contains the given text, like Playwright) with the
// text quoted, closing its compound selector: at the end, or followed by a
// descendant or child part (".card:has-text('Lena') .btn", "li:has-text('x') > a").
func checkHasText(sel string) error {
	if !strings.Contains(sel, ":has-text(") || strings.HasPrefix(sel, "text=") {
		return nil
	}
	for _, part := range splitSelectorList(sel) {
		if !strings.Contains(part, ":has-text(") {
			continue
		}
		locs := hasTextRe.FindAllStringIndex(part, -1)
		if len(locs) != strings.Count(part, ":has-text(") {
			return fmt.Errorf(`:has-text takes quoted text: ".row:has-text('Lena')", not %q`, strings.TrimSpace(part))
		}
		for _, loc := range locs {
			rest := part[loc[1]:]
			if next := strings.TrimLeft(rest, " \t\n"); next == "" || next[0] == '>' || (len(next) < len(rest) && !strings.ContainsAny(next[:1], "+~")) {
				continue
			}
			return fmt.Errorf(`:has-text("...") must close its part of the selector - at the end, or followed by a space or > and the element inside ("%s .btn") - not %q`, strings.TrimSpace(part[:loc[1]]), strings.TrimSpace(part))
		}
	}
	return nil
}

// splitSelectorList splits a selector list on the commas that separate selectors,
// not those inside (), [] or quotes.
func splitSelectorList(sel string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(sel); i++ {
		c := sel[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, sel[start:i])
			start = i + 1
		}
	}
	return append(parts, sel[start:])
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// LoadFlowSource returns the stored of_test.js body, or "" when the project has none.
func LoadFlowSource(db interface {
	QueryRow(string, ...any) *sql.Row
}) string {
	var body sql.NullString
	if err := db.QueryRow(
		`SELECT body FROM of_files WHERE kind = ? AND path = ? AND is_deleted = 0`, FlowKind, FlowPath,
	).Scan(&body); err != nil {
		return ""
	}
	return body.String
}

// flowDateRe matches text that usually depends on the day the flow runs: month and
// weekday names, years, calendar dates and clock times.
var flowDateRe = regexp.MustCompile(`\b(January|February|March|April|May|June|July|August|September|October|November|December|` +
	`Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday|(?:19|20)\d\d|\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}|\d{4}-\d{2}-\d{2}|\d{1,2}:\d{2})\b`)

// Lint returns advisory warnings for assertions that will likely stop holding on
// another day. Flows are replayed after every future build, so an expectation such
// as the month name shown after "Previous month" passes today and fails a correct
// app next month. Advisory only: a static label ("Founded in 1999", a "Monday"
// column header) is legitimate, and only the author knows which one it is. Actions
// are not linted: clicking a "Sunday" option is stable.
func (s *FlowSuite) Lint() []string {
	var out []string
	out = append(out, lintPresenceDespawn(s)...)
	for _, f := range s.Flows {
		out = append(out, lintAsyncPresence(f)...)
		out = append(out, lintProof(f)...)
		for i, st := range f.Steps {
			verb, arg := st.verb()
			switch verb {
			case "expectText", "expectNoText", "expectUrl", "expectJs", "waitFor", "expectVisible", "expectHidden", "expectCount":
			default:
				continue
			}
			if m := flowDateRe.FindString(arg); m != "" {
				out = append(out, fmt.Sprintf("flow %q step %d %s asserts %q, which looks date- or time-dependent: flows re-run on later days, so if this "+
					"text comes from today's date or the clock, the flow will fail a working app. Assert structure instead (expectCount, expectVisible, "+
					"expectChanged, or expectJs comparing against new Date()) and re-save; if %q is static text, ignore this.", f.Name, i+1, st, m, m))
			}
		}
	}
	return out
}

// flowSlowWait is the timeout from which a waitFor/expectVisible signals that its
// author expects a slow async result (a streamed or LLM reply, a server round trip).
const flowSlowWait = 8000

// lintAsyncPresence flags a slow wait that only proves an element EXISTS and is
// never followed by a check that the async work FINISHED. Apps render the reply
// bubble, row or card at once with a spinner or typing indicator inside, so such a
// wait passes before the request returns - and still passes when it fails and the
// app shows its error card. Only a confirming assertion before the next action
// (text, a hidden loader, changed pixels, or a JS condition) proves completion.
func lintAsyncPresence(f Flow) []string {
	var out []string
	for i, st := range f.Steps {
		verb, arg := st.verb()
		if (verb != "waitFor" && verb != "expectVisible") || st.Timeout < flowSlowWait {
			continue
		}
		confirmed := false
	scan:
		for _, next := range f.Steps[i+1:] {
			switch v, _ := next.verb(); v {
			case "expectText", "expectNoText", "expectHidden", "expectChanged", "expectJs":
				confirmed = true
				break scan
			case "waitFor", "expectVisible", "expectCount", "expectUrl", "expectAnimating", "snapshot", "wait":
				continue
			default:
				break scan // the next action: the result was never checked
			}
		}
		if !confirmed {
			out = append(out, fmt.Sprintf("flow %q step %d %s waits %dms for %q to appear but nothing then checks the result finished: "+
				"the element is usually rendered at once with a loading/typing indicator inside, so this passes before the reply arrives "+
				"and even when the request fails. Follow it with the finished state - expectHidden on the loading indicator plus "+
				"expectJs/expectText that the result is non-empty and is not an error message - and re-save.", f.Name, i+1, st, st.Timeout, arg))
		}
	}
	return out
}
