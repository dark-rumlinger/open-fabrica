package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/store"
)

func TestClickChanged(t *testing.T) {
	base := exploreState{Mut: 3, Req: 1, Effect: 0, Href: "http://x/", Canvas: "a"}
	if clickChanged(base, base) {
		t.Fatal("an identical state is no change")
	}
	for name, after := range map[string]exploreState{
		"dom":    {Mut: 4, Req: 1, Href: "http://x/", Canvas: "a"},
		"req":    {Mut: 3, Req: 2, Href: "http://x/", Canvas: "a"},
		"effect": {Mut: 3, Req: 1, Effect: 1, Href: "http://x/", Canvas: "a"},
		"hash":   {Mut: 3, Req: 1, Href: "http://x/#a", Canvas: "a"},
		"scroll": {Mut: 3, Req: 1, Href: "http://x/", SY: 200, Canvas: "a"},
		"canvas": {Mut: 3, Req: 1, Href: "http://x/", Canvas: "b"},
	} {
		if !clickChanged(base, after) {
			t.Errorf("%s: change not seen", name)
		}
	}
}

func TestExploreIssues(t *testing.T) {
	rep := pageReport{
		Page:     "/staff",
		Inert:    []exploreControl{{Tag: "button", Text: "Undo", Sel: "button#undo"}, {Tag: "span", Sel: "span.icon"}},
		Errors:   []clickError{{Control: exploreControl{Tag: "button", Text: "Advance", Sel: "button.adv"}, Errors: []string{"[uncaught] TypeError: x is null"}}},
		Covered:  []coveredControl{{Control: exploreControl{Tag: "button", Text: "Menu", Sel: "button#menu"}, Viewport: "mobile", Cover: "<div.overlay>"}},
		Overflow: []overflowReport{{Viewport: "mobile", Width: 390, Scroll: 640, Offenders: []string{"table.orders ends at 640px"}}},
	}
	got := map[string][]ValidationIssue{}
	for _, is := range exploreIssues(rep) {
		got[is.Category] = append(got[is.Category], is)
	}
	if len(got["click_error"]) != 1 || !strings.Contains(got["click_error"][0].Message, "TypeError") {
		t.Errorf("click_error = %+v", got["click_error"])
	}
	if len(got["inert_click"]) != 2 {
		t.Fatalf("named and unnamed inert controls are separate findings, got %+v", got["inert_click"])
	}
	for _, is := range got["inert_click"] {
		named := strings.Contains(is.Message, "Undo")
		if named != (is.Severity == store.SeverityError) {
			t.Errorf("a labelled control is an error, an unlabelled one a warning: %+v", is)
		}
	}
	if len(got["covered_control"]) != 1 || !strings.Contains(got["covered_control"][0].Message, "mobile") {
		t.Errorf("covered_control = %+v", got["covered_control"])
	}
	if len(got["layout_overflow"]) != 1 || !strings.Contains(got["layout_overflow"][0].Message, "table.orders") {
		t.Errorf("layout_overflow = %+v", got["layout_overflow"])
	}
	if len(exploreIssues(pageReport{Page: "/"})) != 0 {
		t.Error("an empty report has no findings")
	}
	if b := exploreIssues(pageReport{Page: "/", Blank: true}); len(b) != 1 || b[0].Category != "blank_page" {
		t.Errorf("blank page = %+v", b)
	}
}

// exploreFixture serves html at / and explores it in a real headless Chrome.
func exploreFixture(t *testing.T, html string) pageReport {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	t.Cleanup(srv.Close)
	alloc, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{})
	if err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	}
	t.Cleanup(cancel)
	return explorePage(alloc, srv.URL+"/", "/", "")
}

func controlNames(cs []exploreControl) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Text)
	}
	return strings.Join(out, ",")
}

// A working class toggle, a fetch, and an in-page link all do something; a
// handler that only flips a class nothing styles does not, and neither does a
// button with no handler at all.
func TestExploreBrowser_InertAndWorking(t *testing.T) {
	rep := exploreFixture(t, `<!doctype html><html><head><style>
	  .hidden{display:none}
	</style></head><body>
	  <p>A page with enough visible text to not read as blank.</p>
	  <button id="toggle">Show panel</button>
	  <div id="panel" class="hidden">Panel</div>
	  <button id="save">Save</button>
	  <button id="dead">Export</button>
	  <button id="wrong">Open drawer</button>
	  <div id="drawer" class="hidden">Drawer</div>
	  <a href="#panel">Jump</a>
	  <div style="height:2000px"></div>
	  <script>
	    toggle.onclick = () => panel.classList.toggle('hidden');
	    save.onclick = () => { fetch('/api/save', {method: 'POST'}).catch(() => {}); };
	    let x = 0; wrong.onclick = () => { x++; };
	  </script>
	</body></html>`)
	got := controlNames(rep.Inert)
	if got != "Export,Open drawer" {
		t.Fatalf("inert = %q, want Export and Open drawer (clicked %d)", got, rep.Clicked)
	}
	if len(rep.Errors) != 0 || len(rep.Covered) != 0 || len(rep.Overflow) != 0 || rep.Blank {
		t.Fatalf("unexpected findings: %+v", rep)
	}
}

func TestExploreBrowser_ClickErrorAndCovered(t *testing.T) {
	rep := exploreFixture(t, `<!doctype html><html><body>
	  <p>A page with enough visible text to not read as blank.</p>
	  <button id="boom">Advance order</button>
	  <button id="under" style="position:relative">Under the veil</button>
	  <div style="position:fixed;inset:0;z-index:5;background:transparent"></div>
	  <script>boom.onclick = () => { null.go(); };</script>
	</body></html>`)
	if len(rep.Covered) == 0 {
		t.Fatalf("a full-viewport layer covers the buttons: %+v", rep)
	}
	for _, c := range rep.Covered {
		if c.Control.Text == "Under the veil" {
			return
		}
	}
	t.Fatalf("the covered button is not named: %+v", rep.Covered)
}

// Controls inside a closed <details> are not on screen: Chrome still reports a box
// for them, so a click there landed on the list drawn underneath and every one read
// as covered. The model "fixed" that by opening the panel for good, which broke the
// flows that open it. Only its summary is a control until it opens.
func TestExploreBrowser_ClosedDetailsIsNotCovered(t *testing.T) {
	rep := exploreFixture(t, `<!doctype html><html><body>
	  <p>A page with enough visible text to not read as blank.</p>
	  <details id="walkup"><summary>Walk-up order</summary>
	    <button class="chip">Espresso</button><button class="chip">Flat White</button>
	  </details>
	  <ul id="list"><li>Order one</li><li>Order two</li></ul>
	  <script>document.querySelectorAll('.chip').forEach(b => b.onclick = () => list.insertAdjacentHTML('beforeend', '<li>' + b.textContent + '</li>'));</script>
	</body></html>`)
	for _, c := range rep.Covered {
		t.Errorf("a control inside a closed details was reported covered: %+v", c)
	}
	// The same shape through content-visibility, which is how newer Chrome closes a
	// <details>: the hidden subtree still reports a box when asked, the list after it
	// is drawn there, and a click aimed at a chip lands on the list.
	rep = exploreFixture(t, `<!doctype html><html><body>
	  <p>A page with enough visible text to not read as blank.</p>
	  <div id="walkup" style="content-visibility:hidden">
	    <button class="chip">Espresso</button><button class="chip">Flat White</button>
	  </div>
	  <ul id="list"><li>Order one</li><li>Order two</li></ul>
	  <script>document.querySelectorAll('.chip').forEach(b => b.onclick = () => list.insertAdjacentHTML('beforeend', '<li>' + b.textContent + '</li>'));</script>
	</body></html>`)
	for _, c := range rep.Covered {
		t.Errorf("a control in an unpainted subtree was reported covered: %+v", c)
	}
	for _, c := range rep.Inert {
		t.Errorf("a control in an unpainted subtree was clicked: %+v", c)
	}
}

func TestExploreBrowser_ClickError(t *testing.T) {
	rep := exploreFixture(t, `<!doctype html><html><body>
	  <p>A page with enough visible text to not read as blank.</p>
	  <button id="boom">Advance order</button>
	  <script>boom.onclick = () => { null.go(); };</script>
	</body></html>`)
	if len(rep.Errors) != 1 || rep.Errors[0].Control.Text != "Advance order" {
		t.Fatalf("errors = %+v", rep.Errors)
	}
}

func TestExploreBrowser_MobileOverflowAndBlank(t *testing.T) {
	rep := exploreFixture(t, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body>
	  <p>A page with enough visible text to not read as blank.</p>
	  <table id="wide" style="width:900px"><tr><td>wide</td></tr></table>
	</body></html>`)
	var mobile bool
	for _, o := range rep.Overflow {
		if o.Viewport == "mobile" && len(o.Offenders) > 0 {
			mobile = true
		}
		if o.Viewport == "desktop" {
			t.Errorf("900px fits a 1280px desktop: %+v", o)
		}
	}
	if !mobile {
		t.Fatalf("a 900px table scrolls a phone sideways: %+v", rep.Overflow)
	}

	blank := exploreFixture(t, `<!doctype html><html><body><main id="app"></main></body></html>`)
	if !blank.Blank {
		t.Fatalf("an empty mount reads blank: %+v", blank)
	}

	clipped := exploreFixture(t, `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head>
	  <body><p>A page with enough visible text to not read as blank.</p>
	  <div style="overflow-x:hidden"><div style="width:900px">decorative</div></div></body></html>`)
	for _, o := range clipped.Overflow {
		t.Errorf("overflow clipped by an overflow-x:hidden wrapper is not a sideways scroll: %+v", o)
	}
}

// On a canvas page a labelled control that changed nothing in the DOM is only a
// warning: its effect may be on a scene the census cannot read.
func TestExploreIssues_CanvasPageInertIsWarning(t *testing.T) {
	rep := pageReport{Page: "/", Canvas: true, Inert: []exploreControl{{Tag: "button", Text: "Replay", Sel: "button#replay"}}}
	got := exploreIssues(rep)
	if len(got) != 1 || got[0].Severity != store.SeverityWarning || got[0].Deterministic {
		t.Fatalf("canvas page inert = %+v", got)
	}
	if strings.Contains(got[0].Message, "visible feedback anyway") {
		t.Errorf("the message must not ask for decorative feedback: %s", got[0].Message)
	}
}

func TestClickChanged_ValuesAndPixels(t *testing.T) {
	base := exploreState{Vals: "a|", Shot: "x"}
	if !clickChanged(base, exploreState{Vals: "b|", Shot: "x"}) {
		t.Error("a changed form value is a change")
	}
	if !clickChanged(base, exploreState{Vals: "a|", Shot: "y"}) {
		t.Error("changed canvas pixels are a change")
	}
}

// A property write (a slider moved by a button), a file chooser, an intro gate and an
// empty form's submit are all judged correctly.
func TestExploreBrowser_ValuesFilesGatesAndEmptyForms(t *testing.T) {
	rep := exploreFixture(t, `<!doctype html><html><head><style>
	  #intro{position:fixed;inset:0;z-index:20;background:#fff}
	</style></head><body>
	  <section id="intro"><p>Welcome to the app, press start to begin the tour.</p><button id="start">Start</button></section>
	  <p>A page with enough visible text to not read as blank.</p>
	  <input id="range" type="range" min="0" max="10" value="0">
	  <button id="move">Move slider</button>
	  <input id="file" type="file" style="display:none">
	  <button id="pick">Choose a file</button>
	  <form id="f"><input type="text" id="name"><button type="submit" id="send">Send</button></form>
	  <script>
	    start.onclick = () => intro.remove();
	    move.onclick = () => { range.value = 7; };
	    pick.onclick = () => file.click();
	    f.onsubmit = e => e.preventDefault();
	  </script>
	</body></html>`)
	if rep.Gate != "Start" {
		t.Errorf("the intro gate must be dismissed, got %q", rep.Gate)
	}
	if len(rep.Covered) != 0 {
		t.Errorf("a dismissed gate covers nothing: %+v", rep.Covered)
	}
	if got := controlNames(rep.Inert); got != "" {
		t.Errorf("inert = %q, want none", got)
	}
}

// A sign-in page that bounces a signed-in visitor is explored again as an anonymous one,
// so its own controls are checked instead of being skipped as a redirect.
func TestExploreBrowser_RedirectedPageIsExploredAnonymously(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/home" {
			_, _ = w.Write([]byte(`<!doctype html><p>Home page with enough visible text to not read as blank.</p>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><html><body>
		  <p>Sign in page with enough visible text to not read as blank.</p>
		  <button id="dead">Sign in</button>
		  <script>if (localStorage.getItem('auth_token')) location.replace('/home');</script>
		</body></html>`))
	}))
	t.Cleanup(srv.Close)
	alloc, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{})
	if err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	}
	t.Cleanup(cancel)

	if rep := explorePage(alloc, srv.URL+"/login", "/login", "tok"); rep.Redirect != "/home" {
		t.Fatalf("a signed-in visitor must be redirected, got %+v", rep)
	}
	rep := exploreWithFallback(alloc, srv.URL+"/login", "/login", "tok")
	if rep.Redirect != "" || controlNames(rep.Inert) != "Sign in" {
		t.Fatalf("the anonymous pass must explore the page itself, got %+v", rep)
	}
}
