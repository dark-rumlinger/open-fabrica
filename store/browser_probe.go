package store

// Shared runtime-probe primitives, so the authoritative post-build gate
// (probe) and the in-build test_page probe (tools/observability) load
// and observe a page IDENTICALLY - same observation window, same injected
// instrumentation. Homing both here makes "green in test_page" mean what the
// gate means, by construction.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// RuntimeIdleWindow is how long a runtime probe keeps observing after
// navigation before snapshotting. Cold ESM/WebGL init can throw hundreds of ms
// AFTER first paint, so a probe must not stop at paint; both probes wait this
// same window so neither is blind to what the other would catch.
const RuntimeIdleWindow = 1500 * time.Millisecond

// ProbeInstrumentationJS is registered with Page.addScriptToEvaluateOnNewDocument
// so it runs BEFORE any page or platform script (including js/auth.js). Two
// app-agnostic, false-positive-free injections:
//
//  1. Booby-traps envelope fields on Response.prototype: reading `.data`/`.count`
//     etc. straight off a Response (the "forgot to call .json()" bug) is always
//     wrong yet throws nothing, so this getter - recorded to
//     window.__ofProbeViolations and promoted to a hard runtime error after load -
//     is the only way to catch it. No correct code reads `.data` off a Response.
//  2. Wraps requestAnimationFrame with a counter (window.__ofRAF): the
//     render-liveness signal for a canvas/WebGL page whose boot stalls before its
//     rAF loop (see verifyPageRender / stuckBootWarning). Counting only; it
//     changes no behavior and cannot false-positive a correct page.
const ProbeInstrumentationJS = `(function () {
  if (window.__ofProbeViolations) return;
  var V = window.__ofProbeViolations = [];
  var seen = {};
  window.__ofRAF = 0;
  var _raf = window.requestAnimationFrame;
  if (typeof _raf === 'function') {
    window.requestAnimationFrame = function (cb) { window.__ofRAF++; return _raf.call(window, cb); };
  }
  var P = Response.prototype;
  ['data', 'count', 'rows', 'items', 'results', 'records'].forEach(function (k) {
    if (k in P) return;
    Object.defineProperty(P, k, {
      configurable: true,
      get: function () {
        if (!seen[k]) {
          seen[k] = 1;
          try {
            var frames = ((new Error()).stack || '').split('\n').slice(1, 4).join(' | ');
            V.push('page read .' + k + ' off a fetch Response object. apiFetch/fetch resolve to a Response, NOT parsed JSON -- call .json() first: const { ' + k + ' } = await (await apiFetch(path)).json(). ' + frames);
          } catch (e) {}
        }
        return undefined;
      }
    });
  });
})();`

// RuntimeInstrumentationAction returns a chromedp action that registers the
// envelope tripwire (ProbeInstrumentationJS) and, when authToken is non-empty,
// seeds localStorage.auth_token, both to run BEFORE any page or platform script.
// It MUST precede Navigate to take effect on the load. One helper so the gate and
// the in-build probe instrument every page load identically.
func RuntimeInstrumentationAction(authToken string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		inject := ProbeInstrumentationJS
		if authToken != "" {
			// JSON-encode to get a safely-quoted JS string literal.
			if q, err := json.Marshal(authToken); err == nil {
				inject = "try{localStorage.setItem('auth_token'," + string(q) + ");}catch(e){}\n" + inject
			}
		}
		_, err := page.AddScriptToEvaluateOnNewDocument(inject).Do(ctx)
		return err
	})
}
