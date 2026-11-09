package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"openfabrica/store"
)

// Behavioural tests for the platform-owned js/e2ee.js. Everything else in the
// repo checks that file's TEXT: it parses as an ES module, its wire envelope
// matches the e2ee-wire contract, its guide names real actions. Nothing executed
// it, so a handshake that deadlocks on a reconnect, a persisted row no key can
// reopen, or a blob chunk in the wrong shape all shipped green.
//
// The crypto is Web Crypto, so the only place it can run is a browser. These
// drive the real helper in headless Chrome against an in-page stand-in for the
// relay (testdata/e2ee_harness.html) and skip when Chrome is unavailable, the way
// the rest of the probe layer degrades.

//go:embed testdata/e2ee_harness.html
var e2eeHarnessHTML string

// e2eeHarness serves the harness page plus the REAL embedded helper (e2eeHelperJS,
// the same bytes a project receives) and returns a runner for one named scenario.
func e2eeHarness(t *testing.T) func(name string, out any) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/e2ee.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte(e2eeHelperJS))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(e2eeHarnessHTML))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	alloc, cancelAlloc, err := store.LaunchBrowser(context.Background(), store.BrowserConfig{})
	if err != nil {
		t.Skipf("headless browser unavailable: %v", err)
	}
	t.Cleanup(cancelAlloc)

	tabCtx, cancelTab := chromedp.NewContext(alloc)
	t.Cleanup(cancelTab)
	runCtx, cancelRun := context.WithTimeout(tabCtx, 90*time.Second)
	t.Cleanup(cancelRun)

	if err := chromedp.Run(runCtx,
		chromedp.Navigate(srv.URL),
		chromedp.Poll("window.__ready === true", nil, chromedp.WithPollingTimeout(20*time.Second)),
	); err != nil {
		t.Fatalf("loading the e2ee harness: %v", err)
	}

	return func(name string, out any) {
		t.Helper()
		var res struct {
			OK    bool            `json:"ok"`
			Error string          `json:"error"`
			Value json.RawMessage `json:"value"`
		}
		js := "window.__run(" + jsonQuote(name) + ")"
		if err := chromedp.Run(runCtx, chromedp.Evaluate(js, &res, awaitPromise)); err != nil {
			t.Fatalf("scenario %s: %v", name, err)
		}
		if !res.OK {
			t.Fatalf("scenario %s failed in the page: %s", name, res.Error)
		}
		if out != nil {
			if err := json.Unmarshal(res.Value, out); err != nil {
				t.Fatalf("scenario %s result: %v (raw %s)", name, err, res.Value)
			}
		}
	}
}

// awaitPromise makes chromedp resolve the scenario's promise instead of handing
// back the pending object.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The core contract: fail closed before the key exists, the exact envelope the
// relay guard accepts, a round trip, and a verify code both sides agree on.
func TestE2EEBrowser_TwoPartyRoundTrip(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		ThrewBeforeReady bool   `json:"threwBeforeReady"`
		Received         string `json:"received"`
		VerifyMatches    bool   `json:"verifyMatches"`
	}
	run("twoParty", &got)
	if !got.ThrewBeforeReady {
		t.Error("encrypt() before onReady must throw, never fall back to plaintext")
	}
	if got.Received != "hello there" {
		t.Errorf("round trip: got %q, want %q", got.Received, "hello there")
	}
	if !got.VerifyMatches {
		t.Error("both peers must compute the same verifyCode()")
	}
}

// A reload reconnects on the same client id, so the relay suppresses the leave and
// the roster never clears. The incumbent has to treat the returning peer's fresh
// ephemeral key as a re-handshake; ignoring it as a duplicate left the room
// permanently mute behind a correctly fail-closed composer.
func TestE2EEBrowser_TwoPartyReconnect(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		AReadyFires   int    `json:"aReadyFires"`
		ToReturning   string `json:"toReturning"`
		FromReturning string `json:"fromReturning"`
	}
	run("twoPartyReconnect", &got)
	if got.ToReturning != "after reload" {
		t.Errorf("incumbent -> returning peer: got %q, want %q", got.ToReturning, "after reload")
	}
	if got.FromReturning != "reply after reload" {
		t.Errorf("returning peer -> incumbent: got %q, want %q", got.FromReturning, "reply after reload")
	}
	if got.AReadyFires < 2 {
		t.Errorf("onReady must fire again after a re-derive so a gated composer re-enables, fired %d times", got.AReadyFires)
	}
}

// Group rooms: a shared epoch key, and a member that leaves is locked out of what
// is sent after the rotation.
func TestE2EEBrowser_GroupEpochRotation(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		First         []string `json:"first"`
		ReadByB       string   `json:"readByB"`
		ReadByCIsNull bool     `json:"readByCIsNull"`
	}
	run("group", &got)
	for i, v := range got.First {
		if v != "to the whole room" {
			t.Errorf("member %d decrypted %q, want %q", i, v, "to the whole room")
		}
	}
	if got.ReadByB != "members only" {
		t.Errorf("remaining member: got %q, want %q", got.ReadByB, "members only")
	}
	if !got.ReadByCIsNull {
		t.Error("a departed member must not decrypt an epoch minted after it left")
	}
}

// The group half of the reconnect: the roster is unchanged, so only the returning
// member's fresh pub reveals it. Without a re-key the distributor has already
// recorded sending this epoch and never resends, leaving that member unable to read
// the room it is still nominally in.
func TestE2EEBrowser_GroupReconnect(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		Before      string `json:"before"`
		After       string `json:"after"`
		StillPaired string `json:"stillPaired"`
	}
	run("groupReconnect", &got)
	if got.Before != "before the reload" {
		t.Errorf("pre-reload message: got %q", got.Before)
	}
	if got.After != "after the reload" {
		t.Errorf("returning member must read the room again: got %q", got.After)
	}
	if got.StillPaired != "after the reload" {
		t.Errorf("an untouched member must keep reading across the re-key: got %q", got.StillPaired)
	}
}

// ratchet:true destroys each message key after use, so a replay of an already-read
// frame must fail. That is the forward-secrecy guarantee, and it is also why
// archive:true is refused alongside it.
func TestE2EEBrowser_RatchetForwardSecrecy(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		First        string `json:"first"`
		Second       string `json:"second"`
		ReplayIsNull bool   `json:"replayIsNull"`
	}
	run("ratchet", &got)
	if got.First != "one" || got.Second != "two" {
		t.Errorf("ratcheted round trip: got %q/%q, want one/two", got.First, got.Second)
	}
	if !got.ReplayIsNull {
		t.Error("a consumed ratchet message key must be gone, so re-decrypting returns null")
	}
}

// A ratchet frame's index rides the wire, so the catch-up loop must be bounded: an
// out-of-range index has to be dropped, never grind through that many HKDF steps
// (which would hang the tab). Unbounded, this scenario never returns and the harness
// times out; bounded, it returns null in well under a second.
func TestE2EEBrowser_RatchetLargeIndexBounded(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		Normal     string  `json:"normal"`
		HugeIsNull bool    `json:"hugeIsNull"`
		ElapsedMs  float64 `json:"elapsedMs"`
	}
	run("ratchetLargeIndex", &got)
	if got.Normal != "normal" {
		t.Errorf("a normal ratchet frame must still decrypt: got %q", got.Normal)
	}
	if !got.HugeIsNull {
		t.Error("a ratchet frame with an out-of-range index must be dropped (null), not decrypted")
	}
	if got.ElapsedMs > 2000 {
		t.Errorf("decrypt of an absurd ratchet index must be bounded, took %.0fms (an unbounded catch-up would grind through the index)", got.ElapsedMs)
	}
}

// The blob chunk shape the e2ee-wire contract fixes: one opaque base64 string per
// chunk, never an {iv,ct} object (which the relay would read as a chat envelope
// and persist).
func TestE2EEBrowser_BlobChunkShape(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		N                int  `json:"n"`
		ChunkCount       int  `json:"chunkCount"`
		ChunksAreStrings bool `json:"chunksAreStrings"`
		RoundTrips       bool `json:"roundTrips"`
	}
	run("blob", &got)
	if !got.ChunksAreStrings {
		t.Error("each blob chunk must be one base64 STRING (iv prepended to ciphertext), not an {iv,ct} object")
	}
	if got.N != got.ChunkCount || got.N < 2 {
		t.Errorf("meta.n must equal the chunk count and a 5000-byte body at chunkSize 1200 must split: n=%d chunks=%d", got.N, got.ChunkCount)
	}
	if !got.RoundTrips {
		t.Error("decryptBlob must reproduce the original bytes")
	}
}

// The archive lane. Without it, a persisted row is ciphertext nobody can reopen:
// the AAD binds the relay-stamped _sender that the durable write strips, and the
// live key is a per-connection ephemeral. With it, the row reloads from the room
// and password alone.
func TestE2EEBrowser_ArchiveHistoryReopens(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		Live                  string `json:"live"`
		FromHistory           string `json:"fromHistory"`
		FromHistoryUntagged   string `json:"fromHistoryUntagged"`
		DeniedIsNull          bool   `json:"deniedIsNull"`
		LiveLaneHistoryIsNull bool   `json:"liveLaneHistoryIsNull"`
	}
	run("archive", &got)
	if got.Live != "kept for later" {
		t.Errorf("an archive frame must still decrypt live: got %q", got.Live)
	}
	if got.FromHistory != "kept for later" {
		t.Errorf("a stored row must reopen in a fresh session: got %q", got.FromHistory)
	}
	if got.FromHistoryUntagged != "kept for later" {
		t.Errorf("a stored row whose table has no `a` column must still reopen: got %q", got.FromHistoryUntagged)
	}
	if !got.DeniedIsNull {
		t.Error("the wrong password must not open an archived row")
	}
	if !got.LiveLaneHistoryIsNull {
		t.Error("a row written on the live key must be unreadable later - if this ever passes, the archive lane has stopped being the only readable one and the guide is wrong again")
	}
}

func TestE2EEBrowser_ArchiveGroupAndRatchetRefusal(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		FromHistory    string `json:"fromHistory"`
		RatchetRefused bool   `json:"ratchetRefused"`
	}
	run("archiveGroup", &got)
	if got.FromHistory != "group history" {
		t.Errorf("group archive history must reopen: got %q", got.FromHistory)
	}
	if !got.RatchetRefused {
		t.Error("archive:true with ratchet:true must throw at construction, not ship a room whose history is unreadable")
	}
}

// A malicious relay can replay a peer's earlier signed handshake to force a
// re-derive back to that stale ephemeral key, then replay the messages sent under
// it. The two-party replay counter is namespaced by peer pubkey and NOT cleared on
// re-derive, so a replayed old pub lands on its own old high-water mark and the
// replayed message is dropped instead of rendered as fresh. Clearing the window on
// every re-derive (the old behaviour) re-accepted the replay.
func TestE2EEBrowser_TwoPartyReconnectReplayRejected(t *testing.T) {
	run := e2eeHarness(t)
	var got struct {
		FirstKeyMsg    string `json:"firstKeyMsg"`
		SecondKeyMsg   string `json:"secondKeyMsg"`
		ReplayRejected bool   `json:"replayRejected"`
	}
	run("twoPartyReconnectReplay", &got)
	if got.FirstKeyMsg != "first key message" || got.SecondKeyMsg != "second key message" {
		t.Errorf("normal round trips must still work across a reconnect: got %q / %q", got.FirstKeyMsg, got.SecondKeyMsg)
	}
	if !got.ReplayRejected {
		t.Error("a replayed stale handshake + message must be rejected: the reconnect re-derive must not reset the replay window for that pub")
	}
}
