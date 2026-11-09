package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"openfabrica/store"
)

// These exercise the census against a REAL headless Chrome: the pure verdict is
// unit-tested elsewhere, but the JS visibility/actionability pass, the
// getEventListeners backendNodeId join, and delegation detection can only be
// proven end-to-end. Skips when Chrome is unavailable (the whole probe layer
// degrades that way), so CI without a browser stays green.

func censusFixture(t *testing.T, html string) []ValidationIssue {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	t.Cleanup(srv.Close)

	alloc, cancelAlloc, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{})
	if err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	}
	t.Cleanup(cancelAlloc)

	tabCtx, cancelTab := chromedp.NewContext(alloc)
	t.Cleanup(cancelTab)
	runCtx, cancelRun := context.WithTimeout(tabCtx, 20*time.Second)
	t.Cleanup(cancelRun)

	if err := chromedp.Run(runCtx,
		chromedp.Navigate(srv.URL),
		chromedp.Sleep(300*time.Millisecond), // let load/DOMContentLoaded listeners attach
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	return probeDeadControls(runCtx, "/")
}

func onlyCategory(t *testing.T, issues []ValidationIssue) string {
	t.Helper()
	if len(issues) != 1 {
		t.Fatalf("want exactly one finding, got %d: %+v", len(issues), issues)
	}
	return issues[0].Category
}

// A wired button (addEventListener), a delegated button (listener on a container),
// a navigating link, and a form submit are all alive - the census must flag
// nothing.
func TestCensusBrowser_AllControlsWiredIsClean(t *testing.T) {
	html := `<!doctype html><html><body>
	  <button id="wired">Save</button>
	  <div id="list"><button class="row">Row A</button><button class="row">Row B</button></div>
	  <a href="/next">Go</a>
	  <form action="/submit"><button type="submit">Send</button></form>
	  <script>
	    document.getElementById('wired').addEventListener('click', () => {});
	    document.getElementById('list').addEventListener('click', (e) => { void e; }); // delegation
	  </script>
	</body></html>`
	if issues := censusFixture(t, html); len(issues) != 0 {
		t.Fatalf("all controls are wired; census should be clean, got %+v", issues)
	}
}

// One wired button plus one truly dead button: the page still works, so this is a
// dead_control fix-pass finding, NOT an inert page.
func TestCensusBrowser_OneDeadAmongLiveIsDeadControl(t *testing.T) {
	html := `<!doctype html><html><body>
	  <button id="wired">Works</button>
	  <button id="dead">Does Nothing</button>
	  <script>document.getElementById('wired').addEventListener('click', () => {});</script>
	</body></html>`
	issues := censusFixture(t, html)
	if cat := onlyCategory(t, issues); cat != "dead_control" {
		t.Fatalf("want dead_control, got %q (%+v)", cat, issues)
	}
}

// Every visible control is dead: the app loads but nothing works -> inert_page,
// which blocks the deploy.
func TestCensusBrowser_AllDeadIsInertPage(t *testing.T) {
	html := `<!doctype html><html><body>
	  <button>One</button>
	  <button>Two</button>
	  <div role="button">Three</div>
	</body></html>`
	issues := censusFixture(t, html)
	if cat := onlyCategory(t, issues); cat != "inert_page" {
		t.Fatalf("want inert_page, got %q (%+v)", cat, issues)
	}
}

// An inline onclick is a live control (the census must not need a JS-attached
// listener to see it), and a hidden dead button must never be flagged.
func TestCensusBrowser_InlineOnclickLiveAndHiddenIgnored(t *testing.T) {
	html := `<!doctype html><html><body>
	  <button onclick="void 0">Inline</button>
	  <button style="display:none">Hidden Dead</button>
	</body></html>`
	if issues := censusFixture(t, html); len(issues) != 0 {
		t.Fatalf("inline-onclick control is live and a hidden control is not judged; got %+v", issues)
	}
}

// A page-level (document) click listener is global delegation - it catches every
// bubbled click, so a button with no direct listener is still reachable and must
// not be flagged.
func TestCensusBrowser_DocumentDelegationSuppressesFindings(t *testing.T) {
	html := `<!doctype html><html><body>
	  <button data-act="go">Act</button>
	  <script>document.addEventListener('click', (e) => { void e; });</script>
	</body></html>`
	if issues := censusFixture(t, html); len(issues) != 0 {
		t.Fatalf("document-level delegation should suppress findings, got %+v", issues)
	}
}

// The hidden-HUD deadlock, end to end: the join overlay mounts into a slot that
// is a child of a HUD which starts display:none, and the only code unhiding the HUD
// is behind a button inside that overlay. Chrome sees controls with real listeners
// and zero of them visible, which is what the visible-only census cannot report.
func TestCensusFlagsUIMountedInsideHiddenContainer(t *testing.T) {
	issues := censusFixture(t, `<!doctype html><html><head><style>
	  .hidden { display: none !important; }
	  #hud { position: fixed; inset: 0; }
	  #joinSlot { position: fixed; inset: 0; z-index: 10; }
	</style></head><body>
	  <div id="game"></div>
	  <div id="hud" class="hidden">
	    <button id="btnChat">chat</button>
	    <div id="joinSlot"></div>
	  </div>
	  <script>
	    // The overlay renders successfully into a subtree nothing can see.
	    document.getElementById('joinSlot').innerHTML = '<button id="btnJoin">join</button>';
	    document.getElementById('btnJoin').addEventListener('click', function () {
	      document.getElementById('hud').classList.remove('hidden');
	    });
	  </script>
	</body></html>`)
	if got := onlyCategory(t, issues); got != "hidden_ui" {
		t.Fatalf("want hidden_ui for a UI mounted inside a hidden container, got %q (%d issues)", got, len(issues))
	}
}

// The same page with the mount point OUTSIDE the hidden HUD is the correct shape and
// must stay clean: the join button is visible and wired.
func TestCensusCleanWhenMountPointIsOutsideHiddenContainer(t *testing.T) {
	issues := censusFixture(t, `<!doctype html><html><head><style>
	  .hidden { display: none !important; }
	  #joinSlot { position: fixed; inset: 0; z-index: 10; }
	</style></head><body>
	  <div id="game"></div>
	  <div id="hud" class="hidden"><button id="btnChat">chat</button></div>
	  <div id="joinSlot"></div>
	  <script>
	    document.getElementById('joinSlot').innerHTML = '<button id="btnJoin">join</button>';
	    document.getElementById('btnJoin').addEventListener('click', function () {
	      document.getElementById('hud').classList.remove('hidden');
	    });
	  </script>
	</body></html>`)
	if len(issues) != 0 {
		t.Fatalf("a visible, wired join button must be clean, got %+v", issues)
	}
}
