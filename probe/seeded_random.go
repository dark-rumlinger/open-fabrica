package probe

// Seeded randomness. A replay that deals a game's cards, orders a quiz or picks an enemy
// with Math.random is a different program every run: the same flow passes once and fails
// the next, and the model "fixes" a script or an app that was never wrong. Seeding the page's
// Math.random per tab makes a replay repeat itself. The seed is mixed from the visitor and
// the flow, so two visitors of one flow still draw different random ids and never collide.

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// seededRandomJS replaces Math.random with a mulberry32 generator started at seed.
//
// Each document in a tab mixes in how many documents the tab has already loaded (kept in
// sessionStorage), so a goto or reload does not replay the first page's sequence and make
// client-generated ids collide across pages, while a repeated replay still repeats.
const seededRandomJS = `(function () {
  var n = 0;
  try {
    n = (parseInt(sessionStorage.getItem('__ofSeedDoc'), 10) || 0) + 1;
    sessionStorage.setItem('__ofSeedDoc', String(n));
  } catch (e) {}
  var a = (%d + Math.imul(n, 0x9E3779B1)) >>> 0;
  Math.random = function () {
    a = (a + 0x6D2B79F5) | 0;
    var t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
})();`

// seedRandomAction installs the seeded generator before any page script runs. The seed is
// derived from key (the visitor and flow, or the page being explored).
func seedRandomAction(key string) chromedp.Action {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	script := fmt.Sprintf(seededRandomJS, h.Sum32())
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	})
}
