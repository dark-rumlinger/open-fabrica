package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"openfabrica/store"
	"openfabrica/tools"
)

// TestVerifyPageRender_CatchesDeferredPostPaintError covers an error thrown after
// first paint: a page paints immediately, then runs init code that throws and is
// caught-and-logged (console.error) from a /files/ script. The in-build probe idles
// the shared window (store.RuntimeIdleWindow) and injects the shared
// instrumentation, so it catches the boot failure just as the authoritative gate
// does; stopping at first paint would report passed. Needs a headless browser;
// skips cleanly when one is unavailable or under `go test -short`.
func TestVerifyPageRender_CatchesDeferredPostPaintError(t *testing.T) {
	if testing.Short() {
		t.Skip("browser integration test; skipped under -short")
	}

	// The error fires ~250ms after load - past first paint, inside the idle window.
	const appJS = `document.body.innerHTML = '<h1>PAINTED IMMEDIATELY OK</h1>';
setTimeout(function () {
  try { thisIsNotAFunction(); } catch (e) { console.error(e); }
}, 250);`

	mux := http.NewServeMux()
	mux.HandleFunc("/files/app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte(appJS))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><body><script type="module" src="/files/app.js"></script></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("test server port: %v", err)
	}

	ctx := &tools.ToolContext{Ctx: context.Background()}
	report, rerr := verifyPageRender(ctx, "", port, "/", "")
	if rerr != nil {
		if errors.Is(rerr, store.ErrBrowserUnavailable) {
			t.Skip("headless browser unavailable; skipping deferred-error probe test")
		}
		t.Fatalf("verifyPageRender: %v", rerr)
	}

	// The page DID paint - the exact false-green scenario, where a status/paint
	// check alone would call the page healthy.
	if report.visibleChars == 0 {
		t.Fatalf("expected the page to have painted visible content, got %d", report.visibleChars)
	}
	// ...yet the deferred, caught-and-logged /files/ error must now be a HARD fail.
	if _, failed := report.failureReason(); !failed {
		t.Fatalf("deferred post-paint boot failure was not caught (bootFailures=%v exceptions=%v consoleErrors=%v)",
			report.bootFailures, report.exceptions, report.consoleErrors)
	}
	if len(report.bootFailures) == 0 {
		t.Fatalf("expected a bootFailure from the /files/ console.error, got none")
	}
}
