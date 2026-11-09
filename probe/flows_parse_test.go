package probe

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseFlowSource_Valid(t *testing.T) {
	src := `
const email = "{{email}}";
export default {
  flows: [
    { name: "signup", steps: [
      { goto: "/signup" },
      { fill: "#email", value: email },
      { fill: "#password", value: "{{password}}" },
      { click: "text=Create account" },
      { expectUrl: "/dashboard" },
      { expectCount: ".item", count: 0 },
      { press: "Enter", timeout: 2000 },
      { wait: 500 },
    ] },
  ],
};`
	suite, err := ParseFlowSource(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(suite.Flows) != 1 || len(suite.Flows[0].Steps) != 8 {
		t.Fatalf("unexpected suite: %+v", suite)
	}
	if v, a := suite.Flows[0].Steps[1].verb(); v != "fill" || a != "#email" {
		t.Fatalf("step 2 verb = %s %s", v, a)
	}
	if got := *suite.Flows[0].Steps[1].Value; got != "{{email}}" {
		t.Fatalf("value = %q", got)
	}
}

func TestParseFlowSource_Rejects(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"syntax":                {`export default { flows: [ }`, "does not parse"},
		"empty":                 {``, "empty"},
		"no default":            {`const x = 1;`, "no flows"},
		"import":                {`import x from "./a.js"; export default x;`, "must not import"},
		"unknown verb":          {`export default { flows: [{ name: "a", steps: [{ smash: "#x" }] }] }`, "step verbs"},
		"two verbs":             {`export default { flows: [{ name: "a", steps: [{ click: "#x", goto: "/" }] }] }`, "more than one verb"},
		"no verb":               {`export default { flows: [{ name: "a", steps: [{ timeout: 5 }] }] }`, "exactly one of"},
		"fill no value":         {`export default { flows: [{ name: "a", steps: [{ fill: "#x" }] }] }`, "needs a value"},
		"absolute goto":         {`export default { flows: [{ name: "a", steps: [{ goto: "https://x.com" }] }] }`, "site-relative"},
		"dup name":              {`export default { flows: [{ name: "a", steps: [{ goto: "/" }] }, { name: "a", steps: [{ goto: "/" }] }] }`, "used twice"},
		"unknown var":           {`export default { flows: [{ name: "a", steps: [{ fill: "#x", value: "{{fax}}" }] }] }`, "unknown variable"},
		"bad key":               {`export default { flows: [{ name: "a", steps: [{ goto: "/" }, { press: "CapsLock" }] }] }`, "press takes"},
		"stray count":           {`export default { flows: [{ name: "a", steps: [{ click: "#x", count: 2 }] }] }`, "count does not apply"},
		"huge timeout":          {`export default { flows: [{ name: "a", steps: [{ click: "#x", timeout: 999999 }] }] }`, "timeout must be"},
		"no steps":              {`export default { flows: [{ name: "a", steps: [] }] }`, "no steps"},
		"infinite loop":         {`while (true) {} export default {}`, "threw while evaluating"},
		"wait negative":         {`export default { flows: [{ name: "a", steps: [{ wait: -5 }] }] }`, "wait must be"},
		"wait and click":        {`export default { flows: [{ name: "a", steps: [{ wait: 100, click: "#x" }] }] }`, "more than one verb"},
		"bad viewport":          {`export default { flows: [{ name: "a", viewport: "watch", steps: [{ goto: "/" }] }] }`, "viewport must be"},
		"drag no target":        {`export default { flows: [{ name: "a", steps: [{ drag: "#x" }] }] }`, "exactly one of to"},
		"drag both":             {`export default { flows: [{ name: "a", steps: [{ drag: "#x", to: "#y", path: [[0,0],[1,1]] }] }] }`, "exactly one of to"},
		"path range":            {`export default { flows: [{ name: "a", steps: [{ drag: "#x", path: [[0,0],[2,1]] }] }] }`, "fractions"},
		"at on fill":            {`export default { flows: [{ name: "a", steps: [{ fill: "#x", value: "a", at: [0.5, 0.5] }] }] }`, "at does not apply"},
		"hold on click":         {`export default { flows: [{ name: "a", steps: [{ click: "#x", hold: 100 }] }] }`, "hold does not apply"},
		"bad upload":            {`export default { flows: [{ name: "a", steps: [{ upload: "#f", value: "virus.exe" }] }] }`, "upload needs value"},
		"changed first":         {`export default { flows: [{ name: "a", steps: [{ expectChanged: "#c" }] }] }`, "needs an earlier"},
		"changed other session": {`export default { flows: [{ name: "a", steps: [{ goto: "/" }, { snapshot: "#c" }, { expectChanged: "#c", as: "bob" }] }] }`, "needs an earlier"},
		"bad js":                {`export default { flows: [{ name: "a", steps: [{ expectJs: "if (x) {" }] }] }`, "one JS expression"},
		"bad session":           {`export default { flows: [{ name: "a", steps: [{ goto: "/", as: "9lives" }] }] }`, "short lowercase"},
		"count negative":        {`export default { flows: [{ name: "a", steps: [{ expectCount: ".x", count: -1 }] }] }`, "non-negative"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFlowSource(c.src)
			if err == nil {
				t.Fatalf("expected an error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

// A script with several independent mistakes reports them all in one error, so the
// author fixes them in one re-save instead of one round-trip per mistake. Every
// parse or validation failure also carries the compact DSL reference footer.
func TestParseFlowSource_AggregatesErrorsAndCarriesDSLRef(t *testing.T) {
	src := `export default { flows: [{ name: "many", steps: [
		{ goto: "/" },
		{ fill: "#a" },
		{ expectCount: ".x", count: -1 },
		{ upload: "#f", value: "virus.exe" },
	] }] }`
	_, err := ParseFlowSource(src)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"needs a value", "non-negative", "upload needs value"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
	// The DSL footer, so a save written without recalling the guide can be fixed in one pass.
	for _, want := range []string{"Step verbs:", "Qualifiers:", "recall the user_flows guide"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing DSL reference %q:\n%s", want, msg)
		}
	}
}

func TestFlowVarsExpandPerSession(t *testing.T) {
	v := newFlowVarSet()
	main, bob := v.get("main"), v.get("bob")
	got := v.expand("{{email}}|{{ password }}|{{username}}|{{run}}", "main")
	if want := main.Email + "|" + main.Password + "|" + main.Username + "|" + main.Run; got != want {
		t.Fatalf("expand = %q, want %q", got, want)
	}
	if main.Email == bob.Email || main.Username == bob.Username {
		t.Fatal("two sessions must be two different users")
	}
	if got := v.expand("to {{username:bob}}", "main"); got != "to "+bob.Username {
		t.Fatalf("cross-session reference = %q", got)
	}
	if !strings.HasPrefix(bob.Email, "of-test+"+v.run+"-bob@") {
		t.Fatalf("email %q does not carry the run and session", bob.Email)
	}
	if newFlowVarSet().run == v.run {
		t.Fatal("run ids must differ between runs")
	}
	if len(v.all()) != 2 {
		t.Fatalf("accounts = %+v", v.all())
	}
}

func TestParseFlowSource_GenericVerbs(t *testing.T) {
	src := `export default { flows: [{ name: "everything", viewport: "mobile", steps: [
		{ goto: "/" },
		{ click: "#board", at: [0.25, 0.75] },
		{ dblclick: "text=Title" },
		{ hover: "#menu" },
		{ drag: "#card", to: "#done" },
		{ drag: "#canvas", path: [[0.1, 0.1], [0.9, 0.9]] },
		{ scroll: "bottom" },
		{ press: "ArrowLeft", hold: 500 },
		{ upload: "#avatar", value: "image.png" },
		{ snapshot: "#canvas" },
		{ expectChanged: "#canvas" },
		{ expectAnimating: true },
		{ expectJs: "document.title.length > 0" },
		{ expectHidden: ".modal" },
		{ goto: "/", as: "bob" },
		{ fill: "#msg", value: "hi {{username:bob}}", as: "main" },
		{ expectText: "hi", as: "bob" },
	] }] }`
	suite, err := ParseFlowSource(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n := len(suite.Flows[0].Steps); n != 17 {
		t.Fatalf("steps = %d", n)
	}
}

func TestFlowIssueSignatureIsStable(t *testing.T) {
	f := Flow{Name: "add item", Steps: []FlowStep{{Goto: "/"}, {Click: "#add"}}}
	r := &flowRun{flow: f}
	hard := []string{"[uncaught] TypeError: x is not a function\n    at save (http://h:1/files/js/items.js:4:2)"}
	a := r.issue(1, f.Steps[1], flowFailure{"js_error", "boom"}, hard, "/items", "")
	// Same failure, different volatile detail (host, line): same signature.
	hard2 := []string{"[uncaught] TypeError: x is not a function\n    at save (http://other:2/files/js/items.js:9:7)"}
	b := r.issue(1, f.Steps[1], flowFailure{"js_error", "different words"}, hard2, "/items", "")
	if a.Signature != b.Signature {
		t.Fatalf("signature not stable: %q vs %q", a.Signature, b.Signature)
	}
	c := r.issue(0, f.Steps[0], flowFailure{"not_found", "x"}, nil, "/", "")
	if c.Signature == a.Signature {
		t.Fatal("a different step must yield a different signature")
	}
	if a.Category != "flow" || a.File != FlowPath || !a.Deterministic || a.Severity != "error" {
		t.Fatalf("unexpected issue shape: %+v", a)
	}
}

// Assertions that depend on the day the flow runs are flagged; the same words in
// an action, or ordinary text, are not.
func TestFlowSuiteLint_FlagsDateDependentAssertions(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "calendar", steps: [
		{ goto: "/" },
		{ click: "text=Sunday" },
		{ click: "[aria-label=\"Previous month\"]" },
		{ expectText: "August" },
		{ expectText: "Last 30 days" },
		{ expectText: "Updated 14:05" },
		{ expectNoText: "2026-09-21" },
		{ expectText: "1/5" },
	] }] }`)
	if err != nil {
		t.Fatal(err)
	}
	w := suite.Lint()
	if len(w) != 3 {
		t.Fatalf("want 3 warnings (August, 14:05, 2026-09-21), got %d: %q", len(w), w)
	}
	for i, want := range []string{`"August"`, `"14:05"`, `"2026-09-21"`} {
		if !strings.Contains(w[i], want) {
			t.Errorf("warning %d = %q, want it to name %s", i, w[i], want)
		}
	}
}

// A slow wait that only proves a reply bubble exists is flagged; the same wait
// followed by a completion check is not.
func TestFlowSuiteLint_FlagsUnconfirmedAsyncWaits(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [
		{ name: "chat unconfirmed", steps: [
			{ goto: "/" },
			{ fill: "#input", value: "hi" },
			{ press: "Enter" },
			{ waitFor: ".msg.bot", timeout: 15000 },
			{ click: "#share" },
		] },
		{ name: "chat at end", steps: [
			{ goto: "/" },
			{ press: "Enter" },
			{ waitFor: ".msg.bot", timeout: 15000 },
		] },
		{ name: "chat confirmed", steps: [
			{ goto: "/" },
			{ waitFor: ".msg.bot", timeout: 15000 },
			{ expectHidden: ".typing", timeout: 20000 },
			{ click: "#share" },
		] },
		{ name: "quick wait", steps: [
			{ goto: "/" },
			{ waitFor: ".card" },
			{ click: ".card" },
		] },
	] }`)
	if err != nil {
		t.Fatal(err)
	}
	w := suite.Lint()
	if len(w) != 2 || !strings.Contains(w[0], `"chat unconfirmed"`) || !strings.Contains(w[1], `"chat at end"`) {
		t.Fatalf("want warnings for the two unconfirmed chats only, got %q", w)
	}
}

func TestParseFlowSource_Remember(t *testing.T) {
	ok := `export default { flows: [
		{ name: "share", steps: [
			{ goto: "/" },
			{ upload: "#file", value: "notes.txt" },
			{ remember: "shareLink", from: "#link", attr: "href" },
			{ goto: "{{shareLink}}", as: "bob" },
			{ expectText: "notes.txt", as: "bob" },
		] },
		{ name: "later flow reuses it", steps: [{ goto: "{{shareLink}}" }, { fill: "#q", value: "{{shareLink}}" }] },
	] }`
	if _, err := ParseFlowSource(ok); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := map[string]struct{ src, want string }{
		"use before remember": {`export default { flows: [{ name: "a", steps: [{ goto: "{{link}}" }, { remember: "link", from: "#l" }] }] }`, "unknown variable {{link}}"},
		"no from":             {`export default { flows: [{ name: "a", steps: [{ remember: "link" }] }] }`, "remember needs from"},
		"builtin name":        {`export default { flows: [{ name: "a", steps: [{ remember: "email", from: "#e" }] }] }`, "built-in variable"},
		"bad name":            {`export default { flows: [{ name: "a", steps: [{ remember: "Share Link", from: "#e" }] }] }`, "short name"},
		"session qualifier":   {`export default { flows: [{ name: "a", steps: [{ goto: "/" }, { remember: "link", from: "#l" }, { goto: "{{link:bob}}" }] }] }`, "shared by all sessions"},
		"from on click":       {`export default { flows: [{ name: "a", steps: [{ click: "#x", from: "#y" }] }] }`, "from does not apply"},
		"goto bare word":      {`export default { flows: [{ name: "a", steps: [{ goto: "home" }] }] }`, "site-relative"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFlowSource(c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestSitePath(t *testing.T) {
	for in, want := range map[string]string{
		"/f/x7Kq":                              "/f/x7Kq",
		"https://drop.example/f/x7Kq?dl=1#top": "/f/x7Kq?dl=1#top",
		"coral-yarrow.localhost:5733/f/x7Kq":   "/f/x7Kq",
		"http://coral-yarrow.localhost:5733":   "/",
		"  https://drop.example/manage/abc  ":  "/manage/abc",
	} {
		got, ok := sitePath(in)
		if !ok || got != want {
			t.Errorf("sitePath(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := sitePath("not a link"); ok {
		t.Error("plain words must not resolve to a path")
	}
}

func TestParseFlowSource_InScope(t *testing.T) {
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [
		{ goto: "/" }, { expectText: "Order {{run}}", in: "#zone-queued" }, { expectNoText: "x", in: ".row" },
	] }] }`); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Actions and element checks take in too: "the Delete button of MY row".
	suite, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [
		{ goto: "/" },
		{ click: "text=Delete", in: ".row:has-text('Order {{run}}')" },
		{ fill: "input.qty", value: "2", in: ".row:has-text(\"Order {{run}}\")" },
		{ expectCount: ".tag", count: 2, in: ".card, .row:has-text('x')" },
		{ click: ".row:has-text('Lena') .adv" },
		{ waitFor: "li:has-text('x') > a:has-text('Open')" },
	] }] }`)
	if err != nil {
		t.Fatalf("in on actions: %v", err)
	}
	if st := suite.Flows[0].Steps[1]; st.Click != "text=Delete" || st.In != ".row:has-text('Order {{run}}')" {
		t.Fatalf("step = %+v", st)
	}
	for src, want := range map[string]string{
		`{ goto: "/", in: "#y" }`:                                        "in does not apply to goto",
		`{ click: ".row:has-text(a)" }`:                                  ":has-text takes quoted text",
		`{ click: "b", in: ".row:has-text('a'):first" }`:                 "must close its part of the selector",
		`{ click: ".row:has-text('a') + .next" }`:                        "must close its part of the selector",
		`{ expectJs: "document.querySelector('.row:has-text(\"a\")')" }`: "where :has-text is not valid CSS",
	} {
		_, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [{ goto: "/" }, ` + src + `] }] }`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
}

// A flow's needs are the remembered values it reads before capturing them itself;
// selecting one flow pulls in the earlier flows that capture those values.
func TestRememberedNeedsAndSelection(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [
		{ name: "make link", steps: [{ goto: "/" }, { remember: "link", from: "#l", attr: "href" }] },
		{ name: "unrelated", steps: [{ goto: "/about" }] },
		{ name: "make code", steps: [{ goto: "{{link}}" }, { remember: "code", from: "#c" }, { expectText: "{{code}}" }] },
		{ name: "open both", steps: [{ goto: "{{link}}" }, { fill: "#q", value: "{{code}}" }] },
	] }`)
	if err != nil {
		t.Fatal(err)
	}
	f := suite.Flows
	if got := f[2].rememberedNeeds(); !reflect.DeepEqual(got, []string{"link"}) {
		t.Fatalf("needs = %v; a value the flow remembers itself is not a need", got)
	}
	producer := rememberProducers(f)
	got := selectFlows(f, "open both", producer)
	if want := []bool{true, false, true, true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected = %v, want %v (the flow plus its producers, transitively, not unrelated ones)", got, want)
	}
	if all := selectFlows(f, "", producer); !reflect.DeepEqual(all, []bool{true, true, true, true}) {
		t.Fatalf("no name selects every flow, got %v", all)
	}
	vars := newFlowVarSet()
	if name, ok := missingRemembered(f[3], vars); !ok || name != "link" {
		t.Fatalf("missing = %q %v, want link", name, ok)
	}
	vars.remembered["link"], vars.remembered["code"] = "/s/x", "42"
	if _, ok := missingRemembered(f[3], vars); ok {
		t.Fatal("nothing is missing once both are captured")
	}
}

// Unambiguous slips are rewritten to the canonical step and reported; a genuinely
// ambiguous step is still rejected.
func TestParseFlowSource_NormalizesSlips(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "slips", steps: [
		{ visit: "/" },
		{ tap: "#go" },
		{ type: "#q", text: "hello" },
		{ waitForSelector: ".row", timeout: "8000" },
		{ expectText: "#zone-queued", value: "Sarah" },
		{ assertNoText: "Error", within: ".toast" },
		{ expectCount: ".row", equals: 3 },
		{ remember: "code", from: ".code", attr: "textContent" },
		{ sleep: "500ms" },
		{ expectURL: "/done" },
	] }] }`)
	if err != nil {
		t.Fatalf("slips must be normalized, not rejected: %v", err)
	}
	st := suite.Flows[0].Steps
	checks := []struct {
		got, want any
	}{
		{st[0].Goto, "/"},
		{st[1].Click, "#go"},
		{st[2].Type, "#q"}, {derefStr(st[2].Value), "hello"},
		{st[3].WaitFor, ".row"}, {st[3].Timeout, 8000},
		{st[4].ExpectText, "Sarah"}, {st[4].In, "#zone-queued"},
		{st[5].ExpectNoText, "Error"}, {st[5].In, ".toast"},
		{st[6].ExpectCount, ".row"}, {*st[6].Count, 3},
		{st[7].Remember, "code"}, {st[7].Attr, ""},
		{st[8].Wait, 500},
		{st[9].ExpectURL, "/done"},
	}
	for i, c := range checks {
		if c.got != c.want {
			t.Errorf("check %d: got %v, want %v", i, c.got, c.want)
		}
	}
	if len(suite.Notes) < 10 {
		t.Errorf("every rewrite must be reported, got %d notes: %q", len(suite.Notes), suite.Notes)
	}

	// An alias next to its canonical key is ambiguous and stays rejected.
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [{ tap: "#a", click: "#b" }] }] }`); err == nil {
		t.Fatal("tap together with click must be rejected")
	}
}

// wait and waitFor are easy to swap: a number in waitFor is a millisecond delay
// (the wait verb), and a selector-shaped string in wait is an element to wait for
// (waitFor). Each is rewritten to the verb it belongs to rather than rejected.
func TestParseFlowSource_NormalizesWaitWaitFor(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "waits", steps: [
		{ goto: "/" },
		{ waitFor: 500 },
		{ wait: "#done" },
	] }] }`)
	if err != nil {
		t.Fatalf("wait/waitFor slips must be normalized, not rejected: %v", err)
	}
	st := suite.Flows[0].Steps
	if st[1].Wait != 500 || st[1].WaitFor != "" {
		t.Errorf("waitFor: 500 should become {wait: 500}, got wait=%d waitFor=%q", st[1].Wait, st[1].WaitFor)
	}
	if st[2].WaitFor != "#done" || st[2].Wait != 0 {
		t.Errorf(`wait: "#done" should become {waitFor: "#done"}, got waitFor=%q wait=%d`, st[2].WaitFor, st[2].Wait)
	}
}

// The write_flows rejects the builds actually hit, each with a clear intent, are
// rewritten instead of costing a turn: code-style key names ("KeyW"),
// a modifier chord ("Shift+Enter", valid as written), an over-long fixed
// wait (15000), a reload step and a camelCase
// session name ("boardWatcher"). A modifier that does not exist is still
// rejected.
func TestParseFlowSource_NormalizesBuildSlips(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "slips", steps: [
		{ goto: "/" },
		{ press: "KeyW", hold: 2000 },
		{ press: "Digit1" },
		{ press: "Esc" },
		{ press: "Shift+Enter" },
		{ press: "Ctrl+KeyZ" },
		{ wait: 15000 },
		{ reload: true },
		{ refresh: true },
		{ goto: "/board", as: "boardWatcher" },
	] }] }`)
	if err != nil {
		t.Fatalf("slips must be normalized, not rejected: %v", err)
	}
	st := suite.Flows[0].Steps
	checks := []struct {
		got, want any
	}{
		{st[1].Press, "w"}, {st[1].Hold, 2000},
		{st[2].Press, "1"},
		{st[3].Press, "Escape"},
		{st[4].Press, "Shift+Enter"},
		{st[5].Press, "Ctrl+z"},
		{st[6].Wait, 10000},
		{st[7].Reload, true},
		{st[8].Reload, true},
		{st[9].As, "boardwatcher"},
	}
	for i, c := range checks {
		if c.got != c.want {
			t.Errorf("check %d: got %v, want %v", i, c.got, c.want)
		}
	}
	if len(suite.Notes) < 7 {
		t.Errorf("every rewrite must be reported, got %d notes: %q", len(suite.Notes), suite.Notes)
	}
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [{ press: "Hyper+x" }] }] }`); err == nil || !strings.Contains(err.Error(), "not a modifier") {
		t.Fatalf("an unknown modifier must be rejected, got %v", err)
	}
}

// A build's trap: as on the opening goto only. It opened a second user while the
// rest of each flow ran as main on a blank page, so all four flows failed at step 2
// against a working site. The as is dropped (main is already a fresh visitor); a
// session that genuinely acts before opening a page is rejected at save with the
// reason, instead of failing every replay.
func TestParseFlowSource_SessionMustOpenAPageFirst(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "browse", steps: [
		{ goto: "/", as: "mark" },
		{ waitFor: ".home-name" },
		{ click: "text=Work" },
	] }] }`)
	if err != nil {
		t.Fatalf("an opening-only as must be dropped, not rejected: %v", err)
	}
	if as := suite.Flows[0].Steps[0].As; as != "" {
		t.Errorf("opening-only as should be dropped, got %q", as)
	}
	if len(suite.Notes) == 0 || !strings.Contains(strings.Join(suite.Notes, " "), "blank page") {
		t.Errorf("the rewrite must be reported, got %q", suite.Notes)
	}

	_, err = ParseFlowSource(`export default { flows: [{ name: "chat", steps: [
		{ goto: "/chat" },
		{ fill: "#msg", value: "hi" },
		{ expectText: "hi", as: "bob" },
	] }] }`)
	if err == nil || !strings.Contains(err.Error(), "has not opened a page yet") {
		t.Fatalf("a session acting before its goto must be rejected with the reason, got %v", err)
	}

	// Sessions used on purpose, each opening its own page, stay valid.
	if _, err := ParseFlowSource(`export default { flows: [{ name: "two", steps: [
		{ goto: "/chat" }, { goto: "/chat", as: "bob" }, { wait: 100, as: "bob" },
		{ fill: "#msg", value: "hi" }, { expectText: "hi", as: "bob" },
	] }] }`); err != nil {
		t.Fatalf("two sessions that each open a page must parse: %v", err)
	}
}

// Games sprint on Shift and read e.code, so some builds wrote {press: "ShiftLeft"}
// and {press: "Shift"} - both rejected, a turn each. Modifiers are keys of their own
// now, code-style spellings read as their name, and a modifier chord still parses.
func TestParseFlowSource_ModifierKeys(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "sprint", steps: [
		{ goto: "/" },
		{ press: "ShiftLeft", hold: 200 },
		{ press: "Shift" },
		{ press: "Shift+w", hold: 800 },
		{ press: "ControlLeft" },
		{ press: "Ctrl" },
	] }] }`)
	if err != nil {
		t.Fatalf("modifier keys must be accepted: %v", err)
	}
	st := suite.Flows[0].Steps
	for i, want := range []string{"Shift", "Shift", "Shift+w", "Control", "Control"} {
		if got := st[i+1].Press; got != want {
			t.Errorf("step %d: press %q, want %q", i+2, got, want)
		}
	}
}

// A step holding only as (a build's {as: 'bob'} before bob's goto) means "the
// following steps run as bob"; so does as written on the flow itself. Both are
// rewritten instead of rejected, and the session rules still hold afterwards.
func TestParseFlowSource_SessionMarkers(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [
		{ name: "two users", steps: [
			{ goto: "/" },
			{ fill: "#msg", value: "hi" },
			{ as: "bob" },
			{ goto: "/" },
			{ expectText: "hi" },
			{ as: "main" },
			{ expectText: "bob joined" },
		] },
		{ name: "bob alone", as: "bob", steps: [{ goto: "/" }, { expectText: "hi" }] },
	] }`)
	if err != nil {
		t.Fatalf("session markers must be normalized, not rejected: %v", err)
	}
	st := suite.Flows[0].Steps
	if len(st) != 5 {
		t.Fatalf("markers must be dropped, got %d steps: %v", len(st), st)
	}
	for i, want := range []string{"", "", "bob", "bob", ""} {
		if st[i].As != want {
			t.Errorf("step %d runs as %q, want %q", i+1, st[i].As, want)
		}
	}
	for _, s := range suite.Flows[1].Steps {
		if s.As != "bob" {
			t.Errorf("flow-level as must reach every step, got %q", s.As)
		}
	}
	if len(suite.Notes) < 2 {
		t.Errorf("both rewrites must be reported, got %q", suite.Notes)
	}
}

// {goto: "/", expectVisible: "#onboarding"} packs navigation and an
// assertion into one step. The only sensible reading is goto then assert, so it is
// split (as rides on both) instead of costing a turn; three verbs still reject.
func TestParseFlowSource_SplitsGotoPlusVerb(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "first run", steps: [
		{ goto: "/", expectVisible: "#onboarding", timeout: 8000 },
		{ goto: "/", as: "bob", waitFor: ".list" },
	] }] }`)
	if err != nil {
		t.Fatalf("goto plus one verb must be split, not rejected: %v", err)
	}
	st := suite.Flows[0].Steps
	if len(st) != 4 {
		t.Fatalf("want 4 steps after the split, got %d: %v", len(st), st)
	}
	if st[0].Goto != "/" || st[1].ExpectVisible != "#onboarding" || st[1].Timeout != 8000 {
		t.Errorf("first pair split wrong: %v, %v", st[0], st[1])
	}
	if st[2].As != "bob" || st[3].As != "bob" || st[3].WaitFor != ".list" {
		t.Errorf("as must ride on both halves: %v, %v", st[2], st[3])
	}
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [{ goto: "/", click: "#a", expectText: "b" }] }] }`); err == nil {
		t.Error("a step with three verbs must still be rejected")
	}
}

// Playwright habits the model brings along are read as what they mean: a quoted
// text= label, and "outer >> inner" as the inner control inside the outer element.
func TestParseFlowSource_NormalizesPlaywrightSelectors(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [
		{ goto: "/" },
		{ click: 'text="Save"' },
		{ click: ".row:has-text('Lena') >> text='Start brewing'" },
		{ expectVisible: "#list >> .item" },
	] }] }`)
	if err != nil {
		t.Fatal(err)
	}
	st := suite.Flows[0].Steps
	if st[1].Click != "text=Save" {
		t.Errorf("quoted text= = %q", st[1].Click)
	}
	if st[2].Click != "text=Start brewing" || st[2].In != ".row:has-text('Lena')" {
		t.Errorf(">> chain = %+v", st[2])
	}
	if st[3].ExpectVisible != ".item" || st[3].In != "#list" {
		t.Errorf(">> on an assertion = %+v", st[3])
	}
}

// When main never opens a page, steps without as belong to the user whose goto
// came before them (the game build wrote {goto, as: "amy"} then bare steps).
func TestParseFlowSource_InheritsOpeningSession(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "two players", steps: [
		{ goto: "/", as: "amy" },
		{ fill: "#name", value: "Amy" },
		{ click: "#join" },
		{ goto: "/", as: "ben" },
		{ fill: "#name", value: "Ben" },
		{ expectText: "Amy", in: ".roster" },
	] }] }`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"amy", "amy", "amy", "ben", "ben", "ben"}
	for i, st := range suite.Flows[0].Steps {
		if st.As != want[i] {
			t.Errorf("step %d as = %q, want %q", i+1, st.As, want[i])
		}
	}
	if len(suite.Notes) == 0 {
		t.Error("the rewrite must be reported")
	}
	// A flow whose main opens a page keeps its bare steps as main (and a session that
	// acts before its own goto is still rejected).
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [
		{ goto: "/" }, { goto: "/", as: "bob" }, { click: "#x" }, { expectText: "hi", as: "carol" },
	] }] }`); err == nil || !strings.Contains(err.Error(), `runs as "carol"`) {
		t.Fatalf("err = %v", err)
	}
}

// expectChanged without a snapshot gets one before the action that precedes it.
func TestParseFlowSource_InsertsMissingSnapshot(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "play", steps: [
		{ goto: "/" },
		{ expectAnimating: true },
		{ press: "w", hold: 500 },
		{ wait: 100 },
		{ expectChanged: "#game" },
	] }] }`)
	if err != nil {
		t.Fatal(err)
	}
	st := suite.Flows[0].Steps
	if len(st) != 6 || st[2].Snapshot != "#game" || st[3].Press != "w" {
		t.Fatalf("steps = %v", st)
	}
	// Nothing to anchor it to: still rejected with the reason.
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [
		{ goto: "/" }, { expectChanged: "#game" },
	] }] }`); err == nil || !strings.Contains(err.Error(), "needs an earlier {snapshot") {
		t.Fatalf("err = %v", err)
	}
}

// Browser-state verbs: offline, dialog, clock and download, with their checks.
func TestParseFlowSource_BrowserStateVerbs(t *testing.T) {
	suite, err := ParseFlowSource(`export default { flows: [{ name: "a", viewport: "mobile-landscape", steps: [
		{ clock: "2030-01-15" }, { dialog: "dismiss" }, { offline: false },
		{ goto: "/" },
		{ offline: true }, { clock: "+1d" }, { clock: "-90m" },
		{ dialog: "accept", value: "Garden" },
		{ download: "text=Export", value: ".csv", in: ".toolbar" },
		{ offline: false },
	] }] }`)
	if err != nil {
		t.Fatal(err)
	}
	st := suite.Flows[0].Steps
	if st[2].Offline == nil || *st[2].Offline || st[4].Offline == nil || !*st[4].Offline {
		t.Fatalf("offline = %v / %v", st[2].Offline, st[4].Offline)
	}
	if v, _ := st[2].verb(); v != "offline" {
		t.Fatalf("offline:false must still be the step's verb, got %q", v)
	}
	rel, _, _ := parseFlowClock("-90m")
	if rel != -90*time.Minute {
		t.Fatalf("-90m = %v", rel)
	}
	for src, want := range map[string]string{
		`{ dialog: "ok" }`:             `dialog takes "accept" or "dismiss"`,
		`{ clock: "tomorrow" }`:        "clock takes a move",
		`{ download: "#x", count: 2 }`: "count does not apply to download",
	} {
		_, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [{ goto: "/" }, ` + src + `] }] }`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", src, err, want)
		}
	}
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", viewport: "watch", steps: [{ goto: "/" }] }] }`); err == nil {
		t.Error("an unknown viewport must be rejected")
	}
}
