package probe

import (
	"strings"
	"testing"
)

// These check that the flow browser can run the kinds of apps the platform builds
// that need more than DOM events: a camera and WebRTC (video calls), the clipboard
// (share links), location, notifications, touch-only controls, downloads, links that
// open a new tab, confirm/prompt dialogs, a movable clock and offline mode with a
// service worker (PWAs). Real headless Chrome; skipped without one.

const flowEnvPage = `<!doctype html><html><head><meta charset="utf-8"></head><body>
<button id="cam">Camera</button><video id="local" autoplay muted playsinline></video><video id="remote" autoplay muted playsinline></video><p id="camout"></p>
<button id="copy">Copy link</button><p id="copied"></p>
<button id="where">Near me</button><p id="pos"></p>
<button id="tap">Tap</button><p id="touched"></p>
<a id="dl" href="/export.csv" download>Export CSV</a>
<button id="blob">Save notes</button>
<a id="nodl" href="/opened">View page</a>
<a id="open" href="/opened" target="_blank">Open</a>
<button id="del">Delete</button><button id="ask">Rename</button><p id="answer"></p>
<p id="today"></p><button id="refresh">Refresh date</button>
<script>
document.getElementById('cam').addEventListener('click', async function () {
  var stream = await navigator.mediaDevices.getUserMedia({ video: true, audio: true });
  document.getElementById('local').srcObject = stream;
  // A call: two peers in this page, connected over real ICE.
  var a = new RTCPeerConnection(), b = new RTCPeerConnection();
  a.onicecandidate = function (e) { if (e.candidate) b.addIceCandidate(e.candidate); };
  b.onicecandidate = function (e) { if (e.candidate) a.addIceCandidate(e.candidate); };
  b.ontrack = function (e) { document.getElementById('remote').srcObject = e.streams[0]; };
  stream.getTracks().forEach(function (t) { a.addTrack(t, stream); });
  await a.setLocalDescription(await a.createOffer());
  await b.setRemoteDescription(a.localDescription);
  await b.setLocalDescription(await b.createAnswer());
  await a.setRemoteDescription(b.localDescription);
  document.getElementById('camout').textContent = 'tracks ' + stream.getTracks().length;
});
document.getElementById('copy').addEventListener('click', function () {
  navigator.clipboard.writeText('https://app.example/s/abc123').then(function () {
    document.getElementById('copied').textContent = 'Copied';
  });
});
document.getElementById('where').addEventListener('click', function () {
  navigator.geolocation.getCurrentPosition(function (p) {
    document.getElementById('pos').textContent = 'at ' + p.coords.latitude.toFixed(2);
  }, function (e) { document.getElementById('pos').textContent = 'denied ' + e.message; });
});
document.getElementById('tap').addEventListener('touchstart', function () {
  document.getElementById('touched').textContent = 'touched';
});
document.getElementById('blob').addEventListener('click', function () {
  var a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob(['my notes'], { type: 'text/plain' }));
  a.download = 'notes.txt';
  document.body.appendChild(a); a.click(); a.remove();
});
document.getElementById('del').addEventListener('click', function () {
  document.getElementById('answer').textContent = confirm('Delete it?') ? 'deleted' : 'kept';
});
document.getElementById('ask').addEventListener('click', function () {
  document.getElementById('answer').textContent = 'renamed ' + prompt('New name?', 'Untitled');
});
function showDay() { document.getElementById('today').textContent = 'today ' + new Date().toISOString().slice(0, 10); }
showDay();
document.getElementById('refresh').addEventListener('click', showDay);
</script></body></html>`

// flowPWA registers a service worker that serves the page from its cache, so the
// app still opens when the network is gone.
const flowPWA = `<!doctype html><html><head><meta charset="utf-8"></head><body>
<h1>Offline app</h1><p id="net"></p>
<script>
navigator.serviceWorker.register('/sw.js');
function net() { document.getElementById('net').textContent = navigator.onLine ? 'online' : 'offline'; }
net(); addEventListener('online', net); addEventListener('offline', net);
</script></body></html>`

const flowSW = `self.addEventListener('install', function (e) {
  e.waitUntil(caches.open('v1').then(function (c) { return c.add('/pwa'); }).then(function () { return self.skipWaiting(); }));
});
self.addEventListener('activate', function (e) { e.waitUntil(self.clients.claim()); });
self.addEventListener('fetch', function (e) {
  var u = new URL(e.request.url);
  if (u.pathname === '/pwa') e.respondWith(caches.match('/pwa').then(function (r) { return r || fetch(e.request); }));
  if (u.pathname === '/ping') e.respondWith(fetch(e.request));
});`

func envPages() map[string]string {
	return map[string]string{
		"/":       flowEnvPage,
		"/opened": `<!doctype html><h1>Opened in a new tab</h1>`,
		"/pwa":    flowPWA,
	}
}

func TestFlowsEnv_CameraAndWebRTC(t *testing.T) {
	res := runKindsFlow(t, envPages(), `export default { flows: [{ name: "call", steps: [
		{ goto: "/" },
		{ click: "#cam" },
		{ expectText: "tracks 2", in: "#camout" },
		{ expectJs: "document.getElementById('local').videoWidth > 0", timeout: 8000 },
		{ expectJs: "document.getElementById('remote').videoWidth > 0", timeout: 15000 },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("camera and a peer connection must work: %s", res.Issues[0].Message)
	}
}

func TestFlowsEnv_ClipboardLocationNotifications(t *testing.T) {
	res := runKindsFlow(t, envPages(), `export default { flows: [{ name: "grants", steps: [
		{ goto: "/" },
		{ click: "#copy" },
		{ expectText: "Copied", in: "#copied" },
		{ expectJs: "navigator.clipboard.readText().then(t => t.endsWith('/s/abc123'))" },
		{ click: "#where" },
		{ expectText: "at 52.37", in: "#pos" },
		{ expectJs: "Notification.permission === 'granted'" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("granted APIs: %s", res.Issues[0].Message)
	}
}

// On a phone, the flow's taps are touches: a touchstart-only control reacts.
func TestFlowsEnv_TouchOnMobile(t *testing.T) {
	res := runKindsFlow(t, envPages(), `export default { flows: [
		{ name: "phone", viewport: "mobile", steps: [{ goto: "/" }, { click: "#tap" }, { expectText: "touched", in: "#touched" }] },
		{ name: "landscape", viewport: "mobile-landscape", steps: [{ goto: "/" }, { expectJs: "innerWidth > innerHeight" }, { click: "#tap" }, { expectText: "touched" }] },
	] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("touch: %s", res.Issues[0].Message)
	}
}

func TestFlowsEnv_Downloads(t *testing.T) {
	pages := envPages()
	res := runKindsFlowWith(t, pages, map[string]func() (string, string){
		"/export.csv": func() (string, string) { return "text/csv", "name,value\nalpha,1\n" },
	}, `export default { flows: [
		{ name: "export", steps: [{ goto: "/" }, { download: "#dl", value: ".csv" }, { download: "text=Save notes", value: "notes.txt" }] },
		{ name: "not a download", steps: [{ goto: "/" }, { download: "#nodl", timeout: 1500 }] },
	] }`)
	if len(res.Passed) != 1 || res.Passed[0] != "export" {
		t.Fatalf("passed = %v, issues = %+v", res.Passed, res.Issues)
	}
	wantMessage(t, res, "started no download")
}

// A link that opens a new tab takes the flow with it; dialogs answer as armed.
func TestFlowsEnv_PopupsAndDialogs(t *testing.T) {
	res := runKindsFlow(t, envPages(), `export default { flows: [
		{ name: "popup", steps: [{ goto: "/" }, { click: "#open" }, { expectText: "Opened in a new tab" }, { expectUrl: "/opened" }] },
		{ name: "dialogs", steps: [
			{ goto: "/" },
			{ dialog: "dismiss" }, { click: "#del" }, { expectText: "kept", in: "#answer" },
			{ click: "#del" }, { expectText: "deleted", in: "#answer" },
			{ dialog: "accept", value: "Garden" }, { click: "#ask" }, { expectText: "renamed Garden", in: "#answer" },
			{ expectText: "never shown", timeout: 300 },
		] },
	] }`)
	if len(res.Passed) != 1 || res.Passed[0] != "popup" {
		t.Fatalf("passed = %v, issues = %+v", res.Passed, res.Issues)
	}
	wantMessage(t, res, "Last dialog in main: prompt: New name?")
}

func TestFlowsEnv_Clock(t *testing.T) {
	res := runKindsFlow(t, envPages(), `export default { flows: [{ name: "days", steps: [
		{ clock: "2030-01-15T10:00" },
		{ goto: "/" },
		{ expectText: "today 2030-01-15" },
		{ clock: "+1d" },
		{ click: "#refresh" },
		{ expectText: "today 2030-01-16" },
		{ reload: true },
		{ expectText: "today 2030-01-16" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("clock: %s", res.Issues[0].Message)
	}
}

// Offline takes the network down for the page AND its service worker: the cached
// app still opens, and a request the worker passes on fails as it would for a
// person with no connection.
func TestFlowsEnv_OfflinePWA(t *testing.T) {
	res := runKindsFlowWith(t, envPages(), map[string]func() (string, string){
		"/sw.js": func() (string, string) { return "text/javascript", flowSW },
		"/ping":  func() (string, string) { return "text/plain", "pong" },
	}, `export default { flows: [{ name: "offline", steps: [
		{ goto: "/pwa" },
		{ expectJs: "navigator.serviceWorker.ready.then(r => !!r.active)" },
		{ reload: true },
		{ expectJs: "!!navigator.serviceWorker.controller" },
		{ offline: true },
		{ expectText: "offline", in: "#net" },
		{ expectJs: "fetch('/ping').then(() => false, () => true)" },
		{ reload: true },
		{ expectText: "Offline app" },
		{ offline: false },
		{ expectText: "online", in: "#net" },
		{ expectJs: "fetch('/ping').then(r => r.ok)" },
	] }] }`)
	if len(res.Issues) != 0 {
		t.Fatalf("offline: %s", res.Issues[0].Message)
	}
	if !strings.Contains(strings.Join(res.Passed, ","), "offline") {
		t.Fatalf("passed = %v", res.Passed)
	}
}
