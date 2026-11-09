package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"openfabrica/internal/replayid"
	"openfabrica/store"
)

// These drive RunFlowTests against a REAL headless Chrome over fixture pages: the
// step semantics (polling, text= selectors, cross-navigation state, per-step error
// attribution) only mean anything end to end. Skip when Chrome is unavailable.

// flowSite serves a tiny two-page app: /signup posts to /dashboard (a separate
// page load), where "Add" appends an item and "Crash" throws from project code.
const flowSignup = `<!doctype html><html><body>
<form id="f" action="/dashboard" method="get">
  <input id="email" name="email"><input id="password" type="password" name="password">
  <label><input id="terms" type="checkbox"> terms</label>
  <select id="plan"><option value="free">Free</option><option value="pro">Pro plan</option></select>
  <button type="submit">Create account</button>
</form>
<script>
document.getElementById('f').addEventListener('submit', function (e) {
  if (!document.getElementById('terms').checked) { e.preventDefault(); alert('terms'); }
});
</script></body></html>`

const flowDashboard = `<!doctype html><html><body>
<h1>Dashboard</h1>
<input id="title"><button id="add">Add</button><button id="crash">Crash</button><button id="clear">Clear all</button>
<ul id="list"></ul>
<p id="score">Score: 0</p>
<script src="/files/app.js"></script></body></html>`

const flowAppJS = `
document.getElementById('add').addEventListener('click', function () {
  var li = document.createElement('li');
  li.className = 'item';
  // Render after a tick, like a fetch round-trip would.
  setTimeout(function () { li.textContent = document.getElementById('title').value; document.getElementById('list').appendChild(li); }, 200);
});
document.getElementById('clear').addEventListener('click', function () {
  if (confirm('Delete every item?')) { document.getElementById('list').innerHTML = ''; alert('Cleared'); }
});
var score = 0;
document.addEventListener('keydown', function (e) {
  if (e.key === 'ArrowRight') { score++; setTimeout(function () { document.getElementById('score').textContent = 'Score: ' + score; }, 300); }
});
document.getElementById('crash').addEventListener('click', function crashHandler() {
  null.boom();
});`

// flowLab is a grab-bag of non-form interactions: a canvas drawn by pointer drags,
// a rAF game loop (and one that crashes), held keys, native HTML5 drag-and-drop, a
// hover tooltip, double-click editing, a hidden file input and a closable modal.
const flowLab = `<!doctype html><html><body style="margin:0">
<canvas id="pad" width="200" height="120" style="border:1px solid #000"></canvas>
<button id="start">Start</button><button id="start-broken">Start broken</button>
<div id="todo" style="min-height:40px"><span id="card" draggable="true">Card</span></div>
<div id="done" style="min-height:40px;background:#eee"></div>
<span id="tip-anchor">?</span><span id="tip" style="display:none">Help text</span>
<span id="title">Old title</span>
<label for="file">Choose</label><input id="file" type="file" style="display:none"><span id="file-name"></span>
<div id="modal">Modal <button id="close">Close</button></div>
<div id="dropzone" style="padding:20px;border:1px dashed #999">Drop a file here</div><span id="dropped"></span>
<div id="hidden-zone" style="display:none">Hidden drop zone</div>
<script>
var pad = document.getElementById('pad'), g = pad.getContext('2d'), down = false;
window.lastClick = null;
pad.addEventListener('pointerdown', function (e) { down = true; g.beginPath(); g.moveTo(e.offsetX, e.offsetY); });
pad.addEventListener('pointermove', function (e) { if (down) { g.lineTo(e.offsetX, e.offsetY); g.lineWidth = 4; g.stroke(); } });
pad.addEventListener('pointerup', function () { down = false; });
pad.addEventListener('click', function (e) { window.lastClick = [e.offsetX / pad.width, e.offsetY / pad.height]; });
document.getElementById('start').addEventListener('click', function () {
  (function tick() { requestAnimationFrame(tick); })();
});
document.getElementById('start-broken').addEventListener('click', function () {
  var n = 0;
  (function tick() { if (++n > 3) { window.player.move(); } requestAnimationFrame(tick); })();
});
window.heldFor = 0; var downAt = 0;
document.addEventListener('keydown', function (e) { if (e.key === 'ArrowUp' && !downAt) downAt = performance.now(); });
document.addEventListener('keyup', function (e) { if (e.key === 'ArrowUp') { window.heldFor = performance.now() - downAt; downAt = 0; } });
var card = document.getElementById('card'), done = document.getElementById('done');
card.addEventListener('dragstart', function (e) { e.dataTransfer.setData('text/plain', 'card'); });
done.addEventListener('dragover', function (e) { e.preventDefault(); });
done.addEventListener('drop', function (e) { e.preventDefault(); if (e.dataTransfer.getData('text/plain') === 'card') done.appendChild(card); });
document.getElementById('tip-anchor').addEventListener('mouseenter', function () { document.getElementById('tip').style.display = 'inline'; });
document.getElementById('title').addEventListener('dblclick', function () { this.textContent = 'Editing'; });
document.getElementById('file').addEventListener('change', function () { document.getElementById('file-name').textContent = this.files[0].name + ' ' + this.files[0].size; });
document.getElementById('close').addEventListener('click', function () { document.getElementById('modal').style.display = 'none'; });
var zone = document.getElementById('dropzone');
zone.addEventListener('dragover', function (e) { e.preventDefault(); zone.classList.add('over'); });
zone.addEventListener('drop', function (e) {
  e.preventDefault();
  var f = e.dataTransfer.files[0];
  f.text().then(function (txt) { document.getElementById('dropped').textContent = f.name + ' ' + f.type + ' ' + txt.split('\n')[0]; });
});
</script></body></html>`

// flowShare mints a random share link after an async delay, printed as a full URL
// (the way apps show links) and kept in a hidden input too.
const flowShare = `<!doctype html><html><body>
<button id="create">Create link</button>
<p><a id="link"></a></p><input id="code" type="hidden">
<script>
document.getElementById('create').addEventListener('click', function () {
  setTimeout(function () {
    var slug = Math.random().toString(36).slice(2, 9);
    var a = document.getElementById('link');
    a.href = '/s/' + slug;
    a.textContent = location.host + '/s/' + slug;
    document.getElementById('code').value = slug;
  }, 300);
});
</script></body></html>`

// flowForm has buttons whose requests fail on the server (500 and 401) without the
// page throwing, a few named views, and a hidden file input, so failed steps can be
// checked for the context they report.
const flowForm = `<!doctype html><html><head><title>Uploader</title></head><body>
<div id="view-drop"><button id="save">Save</button><button id="secret">Load secret</button><button id="busy">Refresh</button></div>
<div id="view-result">Result area</div>
<label for="fileIn">Choose file</label><input id="fileIn" type="file" style="display:none">
<label class="switch"><input id="agree" type="checkbox" style="display:none"> I agree</label>
<div id="panel" style="display:none">Secret panel</div>
<p id="status"></p>
<script>
document.getElementById('save').addEventListener('click', function () { fetch('/api/save', { method: 'POST' }); });
document.getElementById('secret').addEventListener('click', function () { fetch('/api/secret'); });
document.getElementById('busy').addEventListener('click', function () { fetch('/api/busy'); });
</script></body></html>`

// flowCopy is a page whose text reads differently in the DOM than on screen: a
// label uppercased by CSS, typographic punctuation, and a button that moves to a
// #hash view without a page load.
const flowCopy = `<!doctype html><html><head><meta charset="utf-8"></head><body>
<button id="mute" style="text-transform:uppercase">Sound: on</button>
<button class="chip">Morning walk + add</button>
<h1>This page isn’t here — try again…</h1>
<a id="chat" href="#chat">Chat</a>
</body></html>`

// flowKeys records the last key with its modifiers and counts page loads, so a flow
// can check a chord reached the handler and a reload really reloaded.
const flowKeysPage = `<!doctype html><html><body>
<textarea id="box"></textarea><p id="out"></p><p id="loads"></p>
<script>
var n = Number(sessionStorage.getItem('loads') || 0) + 1; sessionStorage.setItem('loads', n);
document.getElementById('loads').textContent = 'loads: ' + n;
document.getElementById('box').addEventListener('keydown', function (e) {
  document.getElementById('out').textContent = (e.shiftKey ? 'shift+' : '') + (e.ctrlKey ? 'ctrl+' : '') + e.key;
});
</script></body></html>`

// flowCover has a button under a full-page layer whose display rule beats the hidden
// attribute, so the layer is invisible yet swallows every pointer action.
const flowCover = `<!doctype html><html><body>
<button id="under">Delete</button><p id="out"></p>
<div class="veil" hidden style="display:flex;position:fixed;top:0;left:0;right:0;bottom:0"></div>
<script>
document.getElementById('under').addEventListener('click', function () { document.getElementById('out').textContent = 'clicked'; });
</script></body></html>`

// flowReach has controls that look covered to a naive check but are not: a button
// whose label is a child element, a layer that lets pointer events through, and a layer
// that goes away a moment after load.
const flowReach = `<!doctype html><html><body>
<button id="go"><span id="inner">Go</span></button>
<div style="position:fixed;top:0;left:0;right:0;bottom:0;pointer-events:none"></div>
<button id="late">Late</button>
<div id="curtain" style="position:fixed;top:0;left:0;right:0;bottom:0"></div>
<p id="out"></p>
<script>
setTimeout(function () { document.getElementById('curtain').remove(); }, 500);
document.getElementById('go').addEventListener('click', function () { document.getElementById('out').textContent += 'go;'; });
document.getElementById('late').addEventListener('click', function () { document.getElementById('out').textContent += 'late;'; });
</script></body></html>`

// flowDrift has a canvas that redraws itself every frame, so it changes with no input;
// flowStill has one that only changes when a key is pressed.
const flowDrift = `<!doctype html><html><body><canvas id="c" width="80" height="40"></canvas>
<script>
var g = document.getElementById('c').getContext('2d'), n = 0;
(function tick() { n++; g.clearRect(0, 0, 80, 40); g.fillRect(n % 70, 10, 10, 10); requestAnimationFrame(tick); })();
</script></body></html>`

const flowStill = `<!doctype html><html><body><canvas id="c" width="80" height="40"></canvas>
<script>
var g = document.getElementById('c').getContext('2d'), x = 0;
g.fillRect(0, 10, 10, 10);
document.addEventListener('keydown', function () { x += 20; g.clearRect(0, 0, 80, 40); g.fillRect(x, 10, 10, 10); });
</script></body></html>`

// flowRoom is a presence page: each open tab pings the server, and the page shows how
// many tabs pinged recently, so a tab that closes drops out of the count.
const flowRoom = `<!doctype html><html><body><p id="here"></p>
<script>
var id = Math.random().toString(36).slice(2);
function tick() {
  fetch('/api/ping?id=' + id).then(function (r) { return r.text(); }).then(function (n) {
    document.getElementById('here').textContent = n + ' here';
  });
}
tick(); setInterval(tick, 150);
</script></body></html>`

func flowSite(t *testing.T) string {
	t.Helper()
	var mu sync.Mutex
	lastPing := map[string]time.Time{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/room":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowRoom))
		case "/api/ping":
			mu.Lock()
			now := time.Now()
			lastPing[r.URL.Query().Get("id")] = now
			n := 0
			for _, at := range lastPing {
				if now.Sub(at) < 700*time.Millisecond {
					n++
				}
			}
			mu.Unlock()
			_, _ = w.Write([]byte(strconv.Itoa(n)))
		case "/signup":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowSignup))
		case "/dashboard":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowDashboard))
		case "/form":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowForm))
		case "/api/save":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"no such column: code"}`))
		case "/api/busy":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
		case "/whoami":
			// Echoes the rate-limit visitor this tab is counted as (replayid).
			id := ""
			if c, err := r.Cookie(replayid.Cookie); err == nil {
				id = replayid.Identity(c.Value)
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><p id=who>" + id[strings.LastIndex(id, ":")+1:] + "</p>"))
		case "/api/secret":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"sign in first"}`))
		case "/drift":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowDrift))
		case "/still":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowStill))
		case "/upload-ok":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><button id=up>Upload</button><p id=link></p><script>
document.getElementById('up').addEventListener('click', function () {
  fetch('/api/files/upload', { method: 'POST' }).then(function (r) { return r.json(); }).then(function (j) {
    if (j.slug) document.getElementById('link').textContent = 'Share /f/' + j.slug;
  });
});</script>`))
		case "/api/files/upload":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"url":"/files/uploads/photo.jpg","filename":"photo.jpg","size":1000}`))
		case "/logs":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><p id=a>waiting</p><script>console.log("join: connecting"); console.log("join: connecting"); console.warn("join: no welcome yet");</script>`))
		case "/cover":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowCover))
		case "/reach":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowReach))
		case "/share":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowShare))
		case "/copy":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowCopy))
		case "/keys":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowKeysPage))
		case "/lab":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(flowLab))
		case "/files/app.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte(flowAppJS))
		default:
			if strings.HasPrefix(r.URL.Path, "/s/") {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<!doctype html><h1>Shared item " + strings.TrimPrefix(r.URL.Path, "/s/") + "</h1>"))
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runFlowFixture(t *testing.T, src string) FlowResult {
	t.Helper()
	base := flowSite(t)
	if _, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{}); err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	} else {
		cancel()
	}
	suite, err := ParseFlowSource(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := RunFlowTests(context.Background(), suite, base, "", "")
	if res.Skipped != "" {
		t.Fatalf("the replay was skipped although Chrome launched: %s", res.Skipped)
	}
	return res
}

func TestFlowsBrowser_PassingJourneyAcrossNavigation(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "signup and add", steps: [
		{ goto: "/signup" },
		{ fill: "#email", value: "{{email}}" },
		{ fill: "#password", value: "{{password}}" },
		{ check: "#terms" },
		{ select: "#plan", value: "Pro plan" },
		{ click: "text=Create account" },
		{ expectUrl: "/dashboard" },
		{ expectText: "Dashboard" },
		{ fill: "#title", value: "Item {{run}}" },
		{ click: "#add" },
		{ expectText: "Item {{run}}" },
		{ expectCount: ".item", count: 1 },
		{ expectNoText: "Nope" },
		{ click: "text=Clear all" },
		{ expectCount: ".item", count: 0 },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
	if len(res.Passed) != 1 {
		t.Fatalf("passed = %v", res.Passed)
	}
}

func TestFlowsBrowser_ClickThatThrowsFailsThatStep(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "crash", steps: [
		{ goto: "/dashboard" },
		{ click: "#crash" },
		{ expectText: "Dashboard" },
	] }] }`)
	if len(res.Issues) != 1 {
		t.Fatalf("want 1 issue, got %d", len(res.Issues))
	}
	msg := res.Issues[0].Message
	if !strings.Contains(msg, "step 2 of 3") || !strings.Contains(msg, "[uncaught]") {
		t.Fatalf("expected the throw attributed to step 2: %s", msg)
	}
	if !strings.Contains(res.Issues[0].Signature, "js_error") {
		t.Fatalf("signature %q", res.Issues[0].Signature)
	}
}

func TestFlowsBrowser_MissingSelectorTimesOut(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "missing", steps: [
		{ goto: "/dashboard" },
		{ click: "text=Delete everything", timeout: 800 },
	] }] }`)
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Message, `no element matches "text=Delete everything"`) {
		t.Fatalf("unexpected: %+v", res.Issues)
	}
}

func TestFlowsBrowser_ExpectTextMismatch(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "text", steps: [
		{ goto: "/dashboard" },
		{ expectText: "Welcome back", timeout: 600 },
	] }] }`)
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Message, `page does not show "Welcome back"`) {
		t.Fatalf("unexpected: %+v", res.Issues)
	}
}

// Text asserts read the page as a person does: a CSS-uppercased label still shows
// its words, and a script's plain quotes and dashes match typographic ones.
// expectNoText is held to the same reading, so it fails on the uppercased label.
func TestFlowsBrowser_TextFoldsCaseAndPunctuation(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "copy", steps: [
		{ goto: "/copy" },
		{ expectText: "Sound: on", in: "#mute" },
		{ click: "text=Sound: on" },
		{ expectText: "This page isn't here - try again..." },
		{ expectText: "this page", in: "h1" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
	shown := runFlowFixture(t, `export default { flows: [{ name: "still shown", steps: [
		{ goto: "/copy" },
		{ expectNoText: "sound: on", timeout: 500 },
	] }] }`)
	if len(shown.Issues) != 1 || !strings.Contains(shown.Issues[0].Message, `page still shows "sound: on"`) {
		t.Fatalf("unexpected: %+v", shown.Issues)
	}
}

// expectUrl checks the #hash only when the expectation names one, so a hash-routed
// view is assertable and a plain path still ignores the fragment.
func TestFlowsBrowser_ExpectUrlHash(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "hash", steps: [
		{ goto: "/copy" },
		{ click: "#chat" },
		{ expectUrl: "/copy#chat" },
		{ expectUrl: "/copy" },
		{ expectUrl: "/copy#*" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
	wrong := runFlowFixture(t, `export default { flows: [{ name: "wrong hash", steps: [
		{ goto: "/copy" },
		{ click: "#chat" },
		{ expectUrl: "/copy#settings", timeout: 500 },
	] }] }`)
	if len(wrong.Issues) != 1 || !strings.Contains(wrong.Issues[0].Message, "URL is /copy#chat, expected /copy#settings") {
		t.Fatalf("unexpected: %+v", wrong.Issues)
	}
}

func TestFlowsBrowser_OnlyRunsNamedFlow(t *testing.T) {
	base := flowSite(t)
	suite, err := ParseFlowSource(`export default { flows: [
		{ name: "a", steps: [{ goto: "/dashboard" }] },
		{ name: "b", steps: [{ goto: "/dashboard" }, { expectText: "nope", timeout: 300 }] },
	] }`)
	if err != nil {
		t.Fatal(err)
	}
	res := RunFlowTests(context.Background(), suite, base, "", "a")
	if res.Skipped != "" {
		t.Fatalf("the replay was skipped although Chrome launched: %s", res.Skipped)
	}
	if len(res.Passed) != 1 || res.Passed[0] != "a" || len(res.Failed) != 0 {
		t.Fatalf("passed=%v failed=%v", res.Passed, res.Failed)
	}
}

// A game-shaped flow: keys go to the page, a wait lets the game tick, and the HUD
// text is what the flow asserts on.
func TestFlowsBrowser_GameKeysAndWait(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "play", steps: [
		{ goto: "/dashboard" },
		{ press: "ArrowRight" },
		{ press: "ArrowRight" },
		{ wait: 500 },
		{ expectText: "Score: 2", timeout: 300 },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

func TestFlowsBrowser_GenericInteractions(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "lab", steps: [
		{ goto: "/lab" },
		{ snapshot: "#pad" },
		{ drag: "#pad", path: [[0.1, 0.2], [0.5, 0.8], [0.9, 0.2]] },
		{ expectChanged: "#pad" },
		{ click: "#pad", at: [0.25, 0.5] },
		{ expectJs: "window.lastClick && Math.abs(window.lastClick[0] - 0.25) < 0.05" },
		{ click: "#start" },
		{ expectAnimating: true },
		{ press: "ArrowUp", hold: 400 },
		{ expectJs: "window.heldFor >= 350" },
		{ drag: "#card", to: "#done" },
		{ expectJs: "document.getElementById('done').contains(document.getElementById('card'))" },
		{ hover: "#tip-anchor" },
		{ expectVisible: "text=Help text" },
		{ dblclick: "#title" },
		{ expectText: "Editing" },
		{ upload: "text=Choose", value: "image.png" },
		{ expectText: "image.png" },
		{ click: "#close" },
		{ expectHidden: "#modal" },
		{ scroll: "bottom" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

// A canvas game whose loop dies a few frames after starting: nothing on the page
// changes as text, but the flow still fails at the step that started it.
func TestFlowsBrowser_CrashingGameLoopFails(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "broken game", steps: [
		{ goto: "/lab" },
		{ click: "#start-broken" },
		{ wait: 300 },
	] }] }`)
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Message, "step 2 of 3") || !strings.Contains(res.Issues[0].Message, "[uncaught]") {
		t.Fatalf("expected the loop crash to fail the flow: %+v", res.Issues)
	}
}

func TestFlowsBrowser_ExpectAnimatingFailsWhenIdle(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "idle", steps: [
		{ goto: "/lab" },
		{ expectAnimating: true, timeout: 900 },
	] }] }`)
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Message, "not animating") {
		t.Fatalf("unexpected: %+v", res.Issues)
	}
}

// Two sessions are two users: storage written by one is invisible to the other,
// and each flow starts from a clean profile.
func TestFlowsBrowser_SessionsAreIsolatedUsers(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [
		{ name: "two users", steps: [
			{ goto: "/lab" },
			{ expectJs: "(localStorage.setItem('who', 'main'), true)" },
			{ goto: "/lab", as: "bob" },
			{ expectJs: "localStorage.getItem('who') === null", as: "bob" },
			{ expectJs: "localStorage.getItem('who') === 'main'" },
		] },
		{ name: "next flow starts clean", steps: [
			{ goto: "/lab" },
			{ expectJs: "localStorage.getItem('who') === null" },
		] },
	] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected isolated sessions, got: %s", res.Issues[0].Message)
	}
	if len(res.Accounts) != 0 {
		// No step used a variable, so no identities were handed out.
		t.Fatalf("accounts = %+v", res.Accounts)
	}
}

func TestFlowsBrowser_MobileViewport(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "phone", viewport: "mobile", steps: [
		{ goto: "/lab" },
		{ expectJs: "screen.width === 390 && navigator.maxTouchPoints > 0 && /Mobile/.test(navigator.userAgent)" },
		{ click: "#start" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a mobile viewport, got: %s", res.Issues[0].Message)
	}
}

// A runtime-generated link is remembered by one user and opened by another in
// their own session; the same value also works as text and as a URL printed
// without a scheme.
func TestFlowsBrowser_RememberCarriesGeneratedValues(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [
		{ name: "share", steps: [
			{ goto: "/share" },
			{ click: "#create" },
			{ remember: "linkHref", from: "#link", attr: "href" },
			{ remember: "linkText", from: "#link" },
			{ remember: "code", from: "#code" },
			{ goto: "{{linkHref}}", as: "bob" },
			{ expectText: "Shared item {{code}}", as: "bob" },
		] },
		{ name: "printed link in a later flow", steps: [
			{ goto: "{{linkText}}" },
			{ expectText: "Shared item {{code}}" },
		] },
	] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

func TestFlowsBrowser_RememberFailsWhenNothingAppears(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "no link", steps: [
		{ goto: "/share" },
		{ remember: "linkHref", from: "#link", attr: "href", timeout: 600 },
	] }] }`)
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Message, "nothing to remember yet") {
		t.Fatalf("unexpected: %+v", res.Issues)
	}
}

// in: scopes a text assertion to matching elements: the card is in one column,
// not the other, and moves with the drag.
func TestFlowsBrowser_ScopedText(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "columns", steps: [
		{ goto: "/lab" },
		{ expectText: "Card", in: "#todo" },
		{ expectNoText: "Card", in: "#done" },
		{ drag: "#card", to: "#done" },
		{ expectText: "Card", in: "#done" },
		{ expectNoText: "Card", in: "#todo" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
	miss := runFlowFixture(t, `export default { flows: [{ name: "wrong column", steps: [
		{ goto: "/lab" },
		{ expectText: "Card", in: "#done", timeout: 500 },
	] }] }`)
	if len(miss.Issues) != 1 || !strings.Contains(miss.Issues[0].Message, `page does not show "Card" in "#done"`) {
		t.Fatalf("unexpected: %+v", miss.Issues)
	}
}

// upload onto an element that is not a file input drops the file onto it, the
// way a user drags one in from the desktop: the page's drop handler receives a
// real File with the fixture's name, type and contents.
func TestFlowsBrowser_UploadDropsOntoDropZone(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "drop", steps: [
		{ goto: "/lab" },
		{ upload: "#dropzone", value: "notes.txt" },
		{ expectText: "notes.txt text/plain Hello from of_test.js.", in: "#dropped" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
	hidden := runFlowFixture(t, `export default { flows: [{ name: "hidden zone", steps: [
		{ goto: "/lab" },
		{ upload: "#hidden-zone", value: "notes.txt", timeout: 500 },
	] }] }`)
	if len(hidden.Issues) != 1 || !strings.Contains(hidden.Issues[0].Message, "not visible, so nothing can be dropped onto it") {
		t.Fatalf("unexpected: %+v", hidden.Issues)
	}
}

// Running one flow on its own also runs the earlier flow that captures the value it
// needs; when that flow fails first, the dependent flow is blocked, not failed.
func TestFlowsBrowser_RememberDependencies(t *testing.T) {
	base := flowSite(t)
	if _, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{}); err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	} else {
		cancel()
	}
	run := func(src, only string) FlowResult {
		t.Helper()
		suite, err := ParseFlowSource(src)
		if err != nil {
			t.Fatal(err)
		}
		return RunFlowTests(context.Background(), suite, base, "", only)
	}

	ok := `export default { flows: [
		{ name: "make link", steps: [{ goto: "/share" }, { click: "#create" }, { remember: "link", from: "#link", attr: "href" }] },
		{ name: "unrelated", steps: [{ goto: "/dashboard" }] },
		{ name: "open link", steps: [{ goto: "{{link}}" }, { expectText: "Shared item" }] },
	] }`
	res := run(ok, "open link")
	if len(res.Issues) != 0 || !reflect.DeepEqual(res.Passed, []string{"make link", "open link"}) {
		t.Fatalf("running one flow must run its producer first: passed=%v issues=%+v", res.Passed, res.Issues)
	}

	broken := strings.Replace(ok, `{ click: "#create" }, `, ``, 1)
	broken = strings.Replace(broken, `attr: "href" }`, `attr: "href", timeout: 500 }`, 1)
	res = run(broken, "")
	if len(res.Issues) != 1 || !reflect.DeepEqual(res.Failed, []string{"make link"}) {
		t.Fatalf("only the producer fails: failed=%v issues=%d", res.Failed, len(res.Issues))
	}
	if len(res.Blocked) != 1 || !strings.Contains(res.Blocked[0], `open link (needs {{link}}, which flow "make link" failed`) {
		t.Fatalf("the dependent flow must be blocked, got %v", res.Blocked)
	}
}

// A failed step explains itself: the requests that failed with the server's answer,
// the page, the visible controls, the closest match for a missing target, and a
// verdict on whether the app or the step is at fault.
func TestFlowsBrowser_FailuresExplainThemselves(t *testing.T) {
	cases := []struct {
		name, steps string
		want        []string
	}{
		{"server error", `{ click: "#save" }, { expectText: "Saved", timeout: 800 }`, []string{
			`step 2: POST /api/save -> 500: {"error":"no such column: code"}`, "Verdict: the app errored",
		}},
		{"rate limited", `{ click: "#busy" }, { expectText: "Up to date", timeout: 800 }`, []string{
			"step 2: GET /api/busy -> 429", "Verdict: the app's own rate limit refused this visitor (429)", "never raise it just to pass the replay",
		}},
		{"refused request", `{ click: "#secret" }, { expectText: "Welcome", timeout: 800 }`, []string{
			"step 2: GET /api/secret -> 401", "sign in first", "Verdict: a request was refused (4xx)",
		}},
		{"missing target", `{ waitFor: "#view-uploading", timeout: 500 }`, []string{
			`Page at failure: /form ("Uploader")`, `Target "#view-uploading": missing`, `Closest to "#view-uploading": #view-`,
			"Visible controls (usable selectors): ", "#save", "Verdict: nothing errored and the target is not on the page",
		}},
		{"hidden target", `{ expectVisible: "#panel", timeout: 500 }`, []string{
			`Target "#panel": hidden`, "#fileIn (hidden file input)", "Verdict: the target exists but is hidden",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := runFlowFixture(t, `export default { flows: [{ name: "explain", steps: [{ goto: "/form" }, `+c.steps+`] }] }`)
			if len(res.Issues) != 1 {
				t.Fatalf("want one issue, got %+v", res.Issues)
			}
			msg := res.Issues[0].Message
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message lacks %q:\n%s", w, msg)
				}
			}
		})
	}
}

// expectChanged on a canvas that redraws itself passes but is reported as a weak check;
// on a canvas that only changes with input it is clean.
func TestFlowsBrowser_ExpectChangedOnADriftingCanvasIsAWeakCheck(t *testing.T) {
	drifting := runFlowFixture(t, `export default { flows: [{ name: "drift", steps: [
		{ goto: "/drift" }, { snapshot: "#c" }, { press: "ArrowRight" }, { expectChanged: "#c" },
	] }] }`)
	if len(drifting.Issues) != 0 {
		t.Fatalf("the step still passes, got %s", drifting.Issues[0].Message)
	}
	if len(drifting.WeakChecks) != 1 || !strings.Contains(drifting.WeakChecks[0], "changes by itself") {
		t.Errorf("want one weak-check note, got %v", drifting.WeakChecks)
	}

	still := runFlowFixture(t, `export default { flows: [{ name: "still", steps: [
		{ goto: "/still" }, { snapshot: "#c" }, { press: "ArrowRight" }, { expectChanged: "#c" },
	] }] }`)
	if len(still.Issues) != 0 || len(still.WeakChecks) != 0 {
		t.Errorf("a canvas that changes only on input must be clean, got issues=%v weak=%v", still.Issues, still.WeakChecks)
	}
}

// A user who leaves closes their tab: the others see the count drop, and the same user
// can come back with a new goto.
func TestFlowsBrowser_LeaveClosesASession(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "presence", steps: [
		{ goto: "/room" },
		{ goto: "/room", as: "bob" },
		{ expectText: "2 here" },
		{ leave: true, as: "bob" },
		{ expectText: "1 here", timeout: 4000 },
		{ goto: "/room", as: "bob" },
		{ expectText: "2 here" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

// A leave with no open page, or a step as a user who left, is refused at save time.
func TestFlowsParse_LeaveNeedsAnOpenSession(t *testing.T) {
	for name, src := range map[string]string{
		"nothing open": `export default { flows: [{ name: "a", steps: [{ leave: true, as: "bob" }] }] }`,
		"acts after":   `export default { flows: [{ name: "a", steps: [{ goto: "/" }, { goto: "/", as: "bob" }, { leave: true, as: "bob" }, { click: "#x", as: "bob" }] }] }`,
		"leaves twice": `export default { flows: [{ name: "a", steps: [{ goto: "/", as: "bob" }, { leave: true, as: "bob" }, { leave: true, as: "bob" }] }] }`,
	} {
		if _, err := ParseFlowSource(src); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if _, err := ParseFlowSource(`export default { flows: [{ name: "a", steps: [{ goto: "/", as: "bob" }, { leave: true, as: "bob" }, { goto: "/", as: "bob" }] }] }`); err != nil {
		t.Errorf("coming back with a goto must be allowed: %v", err)
	}
}

// A pointer step aimed at a control that another element sits over fails at that step
// and names the element, instead of passing and failing a later step with no cause.
func TestFlowsBrowser_ClickOnCoveredControlNamesTheCover(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "covered", steps: [
		{ goto: "/cover" }, { click: "#under", timeout: 800 }, { expectText: "clicked" },
	] }] }`)
	if len(res.Issues) != 1 {
		t.Fatalf("want one issue, got %+v", res.Issues)
	}
	msg := res.Issues[0].Message
	for _, w := range []string{
		"failed at step 2 of 3", "would land on <div.veil>", "display:flex", "Verdict: another element sits on top",
	} {
		if !strings.Contains(msg, w) {
			t.Errorf("message lacks %q:\n%s", w, msg)
		}
	}
}

// A child element of the target, a layer with pointer-events:none and a layer that goes
// away within the step's timeout are not covers.
func TestFlowsBrowser_ReachableControlsAreNotCovered(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "reach", steps: [
		{ goto: "/reach" },
		{ click: "#go" },
		{ click: "#inner" },
		{ click: "#late", timeout: 3000 },
		{ expectText: "go;go;late;" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

// A checkbox or file input styled away behind its label is used through the label:
// it counts as visible, a click goes to the label, and check still sets the input.
func TestFlowsBrowser_LabelStandsInForHiddenInput(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "stand-ins", steps: [
		{ goto: "/form" },
		{ expectVisible: "#fileIn" },
		{ expectVisible: "#agree" },
		{ click: "#agree" },
		{ expectJs: "document.getElementById('agree').checked" },
		{ uncheck: "#agree" },
		{ expectJs: "!document.getElementById('agree').checked" },
		{ check: "#agree" },
		{ expectJs: "document.getElementById('agree').checked" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

// Each flow session reaches the server as its own visitor for rate limiting: the tab
// sends the replay's token with its flow and session, so two users in one flow and the
// same user in the next flow each get a fresh limit, as separate real visitors would.
func TestFlowsBrowser_SessionsAreSeparateRateLimitVisitors(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [
		{ name: "two users", steps: [
			{ goto: "/whoami" }, { expectText: "0.main", in: "#who" },
			{ goto: "/whoami", as: "bob" }, { expectText: "0.bob", in: "#who", as: "bob" },
		] },
		{ name: "next visitor", steps: [{ goto: "/whoami" }, { expectText: "1.main", in: "#who" }] },
	] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

// A press chord reaches the page's handler with its modifiers set (a composer that
// sends on Enter and breaks the line on Shift+Enter is testable), and reload reloads
// the current page the way the browser button does.
func TestFlowsBrowser_PressChordAndReload(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "keys", steps: [
		{ goto: "/keys" },
		{ click: "#box" },
		{ press: "Shift+Enter" },
		{ expectText: "shift+Enter", in: "#out" },
		{ press: "Control+a" },
		{ expectText: "ctrl+a", in: "#out" },
		{ expectText: "loads: 1", in: "#loads" },
		{ reload: true },
		{ expectText: "loads: 2", in: "#loads" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

// A text= selector that names only part of a label (a build's "text=Morning walk"
// for a chip reading "Morning walk + add") fails with the exact selector to use, so
// the next replay is not spent guessing.
func TestFlowsBrowser_PartialTextSelectorSuggestsWholeLabel(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "chip", steps: [
		{ goto: "/copy" },
		{ click: "text=Morning walk", timeout: 500 },
	] }] }`)
	if len(res.Issues) != 1 {
		t.Fatalf("want one issue, got %+v", res.Issues)
	}
	if msg := res.Issues[0].Message; !strings.Contains(msg, "Did you mean text=Morning walk + add?") {
		t.Errorf("the miss should name the whole label, got:\n%s", msg)
	}
}

// flowCDN serves one ES module the way jsDelivr does: any origin may GET it, but its
// CORS preflight answers without Access-Control-Allow-Headers, so a module fetch that
// carries a custom request header is blocked. Returns the module's URL.
func flowCDN(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte(`export const greet = () => 'hello from the cdn';`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/lib.module.js"
}

// A page whose entry imports a CDN library through the importmap boots in a flow tab
// exactly as it does for a visitor. The replay's identity must never ride a request
// header (CDP sends extra headers cross-origin too, which forces a preflight the CDN
// refuses and kills the whole module graph while the page itself still loads).
func TestFlowsBrowser_CrossOriginModuleLibraryLoads(t *testing.T) {
	lib := flowCDN(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
<script type="importmap">{"imports":{"lib":"` + lib + `"}}</script>
</head><body><p id="out">booting</p>
<script type="module">import { greet } from 'lib'; document.getElementById('out').textContent = greet();</script>
</body></html>`))
	}))
	t.Cleanup(app.Close)
	if _, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{}); err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	} else {
		cancel()
	}
	suite, err := ParseFlowSource(`export default { flows: [{ name: "boot", steps: [
		{ goto: "/" }, { expectText: "hello from the cdn", in: "#out", timeout: 3000 },
	] }] }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := RunFlowTests(context.Background(), suite, app.URL, "", "")
	if res.Skipped != "" {
		t.Fatalf("the replay was skipped although Chrome launched: %s", res.Skipped)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("the CDN module never ran in the flow tab:\n%s", res.Issues[0].Message)
	}
}

// A CDN module the page imports that fails to load (here: served without CORS headers)
// is soft - the project cannot fix a CDN - but it means none of the page's JS ran. The
// red step must name it and say not to restructure the app, rather than report that
// nothing errored (some builds spent their replay budget on exactly that).
func TestFlowsBrowser_BlockedCDNModuleExplainsItself(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript") // no Access-Control-Allow-Origin
		_, _ = w.Write([]byte(`export const greet = () => 'hi';`))
	}))
	t.Cleanup(cdn.Close)
	// localhost resolves to the same loopback server but is another origin than 127.0.0.1.
	lib := strings.Replace(cdn.URL, "127.0.0.1", "localhost", 1) + "/lib.module.js"
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
<script type="importmap">{"imports":{"lib":"` + lib + `"}}</script>
</head><body><div id="landing"></div>
<script type="module">import { greet } from 'lib'; document.getElementById('landing').innerHTML = '<input id="name">';</script>
</body></html>`))
	}))
	t.Cleanup(app.Close)
	if _, cancel, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{}); err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	} else {
		cancel()
	}
	suite, err := ParseFlowSource(`export default { flows: [{ name: "join", steps: [
		{ goto: "/" }, { fill: "#name", value: "x", timeout: 800 },
	] }] }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := RunFlowTests(context.Background(), suite, app.URL, "", "")
	if res.Skipped != "" {
		t.Fatalf("the replay was skipped although Chrome launched: %s", res.Skipped)
	}
	if len(res.Issues) != 1 {
		t.Fatalf("want one issue, got %+v", res.Issues)
	}
	msg := res.Issues[0].Message
	for _, w := range []string{"Scripts that failed to load from another origin:", "lib.module.js", "the page's JS never ran", "do NOT restructure the app"} {
		if !strings.Contains(msg, w) {
			t.Errorf("message lacks %q:\n%s", w, msg)
		}
	}
	if strings.Contains(msg, "nothing errored") {
		t.Errorf("a blocked CDN module must not read as 'nothing errored':\n%s", msg)
	}
}

// A red expectJs reports the value the expression last produced, so a diagnostic
// assertion says what the app state was instead of only "stayed falsy".
func TestFlowsBrowser_ExpectJsReportsLastValue(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "count", steps: [
		{ goto: "/dashboard" },
		{ expectJs: "document.querySelectorAll('.item').length", timeout: 400 },
	] }] }`)
	if len(res.Issues) != 1 {
		t.Fatalf("want one issue, got %+v", res.Issues)
	}
	if msg := res.Issues[0].Message; !strings.Contains(msg, "stayed falsy (last value: 0)") {
		t.Errorf("the failure should carry the last value, got:\n%s", msg)
	}
}

// A game that polls held keys by e.code sees a held modifier: {press: "Shift+w",
// hold} keeps ShiftLeft and KeyW down together (sprint), and a bare Shift is a key.
func TestFlowsBrowser_HeldModifierReachesGameByCode(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "sprint", steps: [
		{ goto: "/keys" },
		{ expectJs: "(window.keys = {}, window.sprinted = false, addEventListener('keydown', e => { keys[e.code] = true; if (keys.ShiftLeft && keys.KeyW) sprinted = true; }), addEventListener('keyup', e => { keys[e.code] = false; }), true)" },
		{ press: "Shift+w", hold: 300 },
		{ expectJs: "window.sprinted" },
		{ expectJs: "!keys.ShiftLeft && !keys.KeyW" },
		{ press: "Shift", hold: 100 },
		{ expectJs: "keys.ShiftLeft === false" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("expected a clean run, got: %s", res.Issues[0].Message)
	}
}

// A red step quotes the API responses the page received, so a 200 that lacks the field
// the page reads (an upload response with no slug) shows up without a console.log and
// a second replay.
func TestFlowsBrowser_FailureQuotesAPIResponses(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "share", steps: [
		{ goto: "/upload-ok" }, { click: "#up" }, { expectText: "Share /f/", timeout: 800 },
	] }] }`)
	if len(res.Issues) != 1 {
		t.Fatalf("want one issue, got %+v", res.Issues)
	}
	msg := res.Issues[0].Message
	want := `API responses in the last steps (what the page received):` + "\n" +
		`  - step 2: POST /api/files/upload -> 201: {"url":"/files/uploads/photo.jpg","filename":"photo.jpg","size":1000}`
	if !strings.Contains(msg, want) {
		t.Errorf("message lacks the API response:\n%s", msg)
	}
}

// A red step reports the page's console output, so a failure that throws nothing (a
// promise that never settles) can still be traced through the app's own log lines.
func TestFlowsBrowser_FailureReportsConsoleOutput(t *testing.T) {
	res := runFlowFixture(t, `export default { flows: [{ name: "silent", steps: [
		{ goto: "/logs" }, { expectText: "connected", timeout: 800 },
	] }] }`)
	if len(res.Issues) != 1 {
		t.Fatalf("want one issue, got %+v", res.Issues)
	}
	msg := res.Issues[0].Message
	for _, w := range []string{"Page console output", "[log] join: connecting", "[warning] join: no welcome yet"} {
		if !strings.Contains(msg, w) {
			t.Errorf("message lacks %q:\n%s", w, msg)
		}
	}
	if strings.Count(msg, "join: connecting") != 1 {
		t.Errorf("a line repeated back to back must appear once:\n%s", msg)
	}
}
