package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// onMessage is documented as the raw catch-all for SERVER frames. It must see each
// parsed frame exactly once and never the helper's own synthetic events (the roster
// array, {} for open/close/error, the bare state object), or a handler written for
// frames (e2ee.handleSignal, m.type switches) is fed values that are not frames.
// Runs the real helper under node with a stub WebSocket; skipped when node is absent.
func TestOfnetOnMessageSeesOnlyServerFrames(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "ofnet.mjs")
	if err := os.WriteFile(helper, []byte(ofnetHelperJS), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
globalThis.location = { host: 'x.test', protocol: 'https:' };
globalThis.window = { sessionStorage: { getItem() { return null; }, setItem() {} }, location: globalThis.location };
globalThis.localStorage = { getItem() { return null; } };
let sock;
globalThis.WebSocket = class {
  constructor(url) { this.readyState = 0; sock = this; }
  send() {}
  close() {}
};
const { connect } = await import('./ofnet.mjs');
const net = connect('game', { room: 'r' });
const seen = [];
const typed = [];
net.onMessage((m) => seen.push(m));
net.on('chat', (m) => typed.push(m.text));
net.onPeers((p) => typed.push('peers:' + p.length));
net.onState((s) => typed.push('state:' + s.n));
sock.readyState = 1;
sock.onopen();
const frames = [
  { _type: 'welcome', _clientId: 'me', _peers: [{ id: 'a', meta: null }] },
  { type: 'chat', text: 'hi' },
  { _type: 'state', state: { n: 1 }, rev: 1 },
];
for (const f of frames) sock.onmessage({ data: JSON.stringify(f) });
sock.onmessage({ data: 'not json' });
const out = { seen, typed };
console.log(JSON.stringify(out));
`
	drv := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(drv, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, drv)
	cmd.Dir = dir
	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, outBytes)
	}
	out := strings.TrimSpace(string(outBytes))
	// Exactly the three server frames, as received: the welcome, the chat, the state
	// frame itself (not the bare state object), and nothing synthetic.
	const want = `{"seen":[{"_type":"welcome","_clientId":"me","_peers":[{"id":"a","meta":null}]},{"type":"chat","text":"hi"},{"_type":"state","state":{"n":1},"rev":1}],"typed":["peers:1","hi","state:1"]}`
	if out != want {
		t.Fatalf("onMessage saw the wrong events\n got: %s\nwant: %s", out, want)
	}
}
