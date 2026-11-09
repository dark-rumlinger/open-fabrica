package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openfabrica/store"
)

// These cover the kinds of UI the first flow fixtures do not: records in rows that
// look alike (act on MY row), live columns, toasts, fields with a maxlength, rich-text
// editors and per-key inputs, and async checks. Each drives a real headless Chrome
// and skips without one.

// flowBoard is an order board: rows with the same buttons, columns an order moves
// through, a toast that keeps a deleted name on screen, a short name field, a
// contenteditable editor that reads its input events, and three one-digit code boxes
// that jump to the next box as a person types.
const flowBoard = `<!doctype html><html><head><meta charset="utf-8"></head><body>
<div id="list">
  <div class="row"><b>Maya</b> <span class="st">Queued</span> <button class="adv">Start</button> <button class="del">Delete</button></div>
  <div class="row"><b>Lena</b> <span class="st">Queued</span> <button class="adv">Start</button> <button class="del">Delete</button></div>
</div>
<section class="col" id="brewing"><h2>Brewing</h2></section>
<section class="col" id="ready"><h2>Ready</h2></section>
<p id="toast"></p>
<input id="name" maxlength="8"><button id="save">Save</button><p id="saved"></p>
<div id="ed" contenteditable="true" style="min-height:20px;border:1px solid #999"></div><p id="edout"></p>
<div id="otp"><input class="d" maxlength="1"><input class="d" maxlength="1"><input class="d" maxlength="1"></div><p id="code"></p>
<script>
document.getElementById('list').addEventListener('click', function (e) {
  var row = e.target.closest('.row'); if (!row) return;
  var name = row.querySelector('b').textContent;
  if (e.target.classList.contains('adv')) {
    setTimeout(function () {
      row.querySelector('.st').textContent = 'Brewing';
      var card = document.createElement('div'); card.className = 'card'; card.textContent = name;
      document.getElementById('brewing').appendChild(card);
    }, 100);
  }
  if (e.target.classList.contains('del')) {
    row.remove();
    document.getElementById('toast').textContent = 'Deleted ' + name + ' - Undo';
  }
});
document.getElementById('save').addEventListener('click', function () {
  document.getElementById('saved').textContent = document.getElementById('name').value;
});
document.getElementById('ed').addEventListener('input', function () {
  document.getElementById('edout').textContent = 'typed: ' + this.innerText;
});
var boxes = document.querySelectorAll('#otp .d');
boxes.forEach(function (b, i) {
  b.addEventListener('input', function () {
    if (i + 1 < boxes.length) boxes[i + 1].focus();
    document.getElementById('code').textContent = 'code ' + Array.prototype.map.call(boxes, function (x) { return x.value; }).join('');
  });
});
</script></body></html>`

// runKindsFlow replays src against a site serving the given pages.
func runKindsFlow(t *testing.T, pages map[string]string, src string) FlowResult {
	t.Helper()
	return runKindsFlowWith(t, pages, nil, src)
}

// runKindsFlowWith also serves other files: path -> (content type, body).
func runKindsFlowWith(t *testing.T, pages map[string]string, files map[string]func() (string, string), src string) FlowResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := files[r.URL.Path]; ok {
			ctype, body := f()
			w.Header().Set("Content-Type", ctype)
			_, _ = w.Write([]byte(body))
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	if _, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{}); err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	} else {
		cancel()
	}
	suite, err := ParseFlowSource(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := RunFlowTests(context.Background(), suite, srv.URL, "", "")
	if res.Skipped != "" {
		t.Fatalf("the replay was skipped although Chrome launched: %s", res.Skipped)
	}
	return res
}

func wantMessage(t *testing.T, res FlowResult, parts ...string) {
	t.Helper()
	if len(res.Issues) != 1 {
		t.Fatalf("want 1 issue, got %d: %+v", len(res.Issues), res.Issues)
	}
	msg := res.Issues[0].Message
	for _, p := range parts {
		if !strings.Contains(msg, p) {
			t.Errorf("message lacks %q:\n%s", p, msg)
		}
	}
}

// in: on an action picks the control inside MY row, and :has-text picks the row.
func TestFlowsKinds_ScopedClickHitsTheRightRow(t *testing.T) {
	res := runKindsFlow(t, map[string]string{"/": flowBoard}, `export default { flows: [{ name: "advance Lena", steps: [
		{ goto: "/" },
		{ click: "text=Start", in: ".row:has-text('Lena')" },
		{ expectText: "Lena", in: "#brewing" },
		{ expectText: "Brewing", in: ".row:has-text('Lena')" },
		{ expectText: "Queued", in: ".row:has-text('Maya')" },
		{ expectCount: ".card", count: 1, in: "#brewing" },
		{ click: ".row:has-text('Maya') .adv" },
		{ expectText: "Brewing", in: ".row:has-text('Maya') > .st" },
		{ expectCount: ".row:has-text('Maya') .del", count: 1 },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("scoped click: %s", res.Issues[0].Message)
	}
}

// A text step that misses says what the scope shows and where the text is instead,
// so a card in the wrong column reads as an app bug rather than a guess.
func TestFlowsKinds_TextMissShowsScopeAndWhereTheTextIs(t *testing.T) {
	res := runKindsFlow(t, map[string]string{"/": flowBoard}, `export default { flows: [{ name: "wrong column", steps: [
		{ goto: "/" },
		{ click: "text=Start", in: ".row:has-text('Lena')" },
		{ expectText: "Lena", in: "#ready", timeout: 800 },
	] }] }`)
	wantMessage(t, res,
		`Inside "#ready" (1 visible) the page shows: #ready "Ready"`,
		`"Lena" IS on the page, outside "#ready"`,
		`Verdict: the text is on the page, but not inside the in: element`)
}

// expectNoText names the element still showing the text (a toast here), which is the
// difference between "the delete never happened" and "scope the step".
func TestFlowsKinds_NoTextNamesWhereItStillShows(t *testing.T) {
	res := runKindsFlow(t, map[string]string{"/": flowBoard}, `export default { flows: [{ name: "delete", steps: [
		{ goto: "/" },
		{ click: "text=Delete", in: ".row:has-text('Lena')" },
		{ expectNoText: "Lena", in: "#list" },
		{ expectNoText: "Lena", timeout: 600 },
	] }] }`)
	wantMessage(t, res, `"Lena" is still shown in: #toast "Deleted Lena - Undo"`, "then scope the step with in:")
}

// fill respects maxlength the way typing does, and the failure it causes names both
// the cut and the closest text on the page.
func TestFlowsKinds_FillHonorsMaxlength(t *testing.T) {
	res := runKindsFlow(t, map[string]string{"/": flowBoard}, `export default { flows: [{ name: "long name", steps: [
		{ goto: "/" },
		{ fill: "#name", value: "Alexandra" },
		{ click: "#save" },
		{ expectText: "Alexandra", in: "#saved", timeout: 600 },
	] }] }`)
	wantMessage(t, res,
		`step 2: "#name" accepts at most 8 characters (maxlength)`,
		`it holds "Alexandr"`,
		`the closest text is in #saved "Alexandr"`)
}

// A contenteditable editor gets the text through the browser's editing (its input
// events fire), and type sends real keys, so boxes that jump on input fill in turn.
func TestFlowsKinds_EditorAndPerKeyTyping(t *testing.T) {
	res := runKindsFlow(t, map[string]string{"/": flowBoard}, `export default { flows: [{ name: "typing", steps: [
		{ goto: "/" },
		{ fill: "#ed", value: "Hello editor" },
		{ expectText: "typed: Hello editor", in: "#edout" },
		{ type: "#otp .d", value: "123" },
		{ expectText: "code 123", in: "#code" },
		{ type: "#name", value: "Bo" },
		{ type: "#name", value: "Cy" },
		{ expectJs: "document.getElementById('name').value === 'Cy'" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("typing: %s", res.Issues[0].Message)
	}
}

// expectJs awaits a promise and tests what it resolves to: a pending promise is
// truthy, so without awaiting every async check passed.
func TestFlowsKinds_ExpectJsAwaitsPromises(t *testing.T) {
	res := runKindsFlow(t, map[string]string{"/": flowBoard}, `export default { flows: [
		{ name: "resolves true", steps: [{ goto: "/" }, { expectJs: "new Promise(r => setTimeout(() => r(42), 50))" }] },
		{ name: "resolves false", steps: [{ goto: "/" }, { expectJs: "Promise.resolve(0)", timeout: 400 }] },
		{ name: "never settles", steps: [{ goto: "/" }, { expectJs: "new Promise(() => {})", timeout: 400 }] },
	] }`)
	if !strings.Contains(strings.Join(res.Passed, ","), "resolves true") {
		t.Fatalf("passed = %v, issues = %+v", res.Passed, res.Issues)
	}
	if len(res.Issues) != 2 {
		t.Fatalf("want the false and pending promises red, got %+v", res.Issues)
	}
	if !strings.Contains(res.Issues[0].Message, "last value: 0") {
		t.Errorf("resolved value not reported: %s", res.Issues[0].Message)
	}
	if !strings.Contains(res.Issues[1].Message, "never settled") {
		t.Errorf("pending promise not reported: %s", res.Issues[1].Message)
	}
}

// Controls inside a web component's open shadow root are reachable like any other.
func TestFlowsKinds_ShadowDOM(t *testing.T) {
	page := `<!doctype html><html><body><fancy-counter id="fc"></fancy-counter>
<script>
customElements.define('fancy-counter', class extends HTMLElement {
  connectedCallback() {
    var root = this.attachShadow({ mode: 'open' }), n = 0;
    root.innerHTML = '<button class="inc">Add one</button><span class="n">0</span>';
    root.querySelector('.inc').addEventListener('click', function () { root.querySelector('.n').textContent = String(++n); });
  }
});
</script></body></html>`
	res := runKindsFlow(t, map[string]string{"/": page}, `export default { flows: [{ name: "shadow", steps: [
		{ goto: "/" },
		{ click: "text=Add one" },
		{ click: ".inc" },
		{ expectText: "2", in: "#fc" },
		{ expectCount: ".n", count: 1 },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("shadow DOM: %s", res.Issues[0].Message)
	}
}
