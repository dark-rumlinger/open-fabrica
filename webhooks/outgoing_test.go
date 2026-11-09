package webhooks

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/netguard"
	"openfabrica/security"
)

// TestHandleEventBoundsConcurrency verifies the fan-out semaphore: firing an
// event with far more subscribed targets than maxConcurrentDeliveries must
// still deliver to every target, while never exceeding the in-flight cap. A
// regression to a `for { go deliver }` would push observed concurrency
// up to the target count.
func TestHandleEventBoundsConcurrency(t *testing.T) {
	const targets = 40

	var inFlight, maxInFlight, delivered int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			m := atomic.LoadInt64(&maxInFlight)
			if cur <= m || atomic.CompareAndSwapInt64(&maxInFlight, m, cur) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond) // hold the slot so overlap is observable
		atomic.AddInt64(&inFlight, -1)
		atomic.AddInt64(&delivered, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr := db.NewProjectDBManager(t.TempDir())
	defer mgr.CloseAll()
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	for i := 0; i < targets; i++ {
		res, err := pdb.ExecWrite(
			"INSERT INTO of_webhooks (name, url, is_enabled, direction) VALUES (?, ?, 1, 'outgoing')",
			"hook-"+string(rune('A'+i%26))+string(rune('0'+i/26)), srv.URL)
		if err != nil {
			t.Fatalf("seed webhook %d: %v", i, err)
		}
		id, _ := res.LastInsertId()
		if _, err := pdb.ExecWrite(
			"INSERT INTO of_webhook_subscriptions (webhook_id, event_type) VALUES (?, ?)",
			id, "test.event"); err != nil {
			t.Fatalf("seed subscription %d: %v", i, err)
		}
	}

	d := &Dispatcher{
		projectDBMgr: mgr,
		logger:       slog.New(slog.DiscardHandler),
		client:       &http.Client{Timeout: 5 * time.Second},
		sem:          make(chan struct{}, maxConcurrentDeliveries),
		// The fixture server is on loopback, which the SSRF guard blocks by
		// default; allow it so this test exercises the delivery semaphore.
		allowPrivateHosts: true,
	}

	d.handleEvent(events.Event{Type: events.EventType("test.event"), ProjectID: 1})

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&delivered) < targets {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: delivered %d/%d", atomic.LoadInt64(&delivered), targets)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := atomic.LoadInt64(&maxInFlight); got > maxConcurrentDeliveries {
		t.Errorf("in-flight deliveries reached %d, exceeds cap of %d", got, maxConcurrentDeliveries)
	}
	if got := atomic.LoadInt64(&maxInFlight); got < 2 {
		t.Errorf("expected overlapping deliveries (saturated cap), only saw %d in flight", got)
	}
}

// TestDeliverBlocksSSRFToPrivateHost covers the SSRF-block regression: with the default
// (secure) Dispatcher, a webhook URL pointing at a loopback/private address must
// be refused before any HTTP request is made, so it cannot be used to reach the
// admin plane or cloud metadata from the server.
func TestDeliverBlocksSSRFToPrivateHost(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr := db.NewProjectDBManager(t.TempDir())
	defer mgr.CloseAll()
	if _, err := mgr.Create(1); err != nil {
		t.Fatalf("create project db: %v", err)
	}

	d := &Dispatcher{
		projectDBMgr: mgr,
		logger:       slog.New(slog.DiscardHandler),
		client:       netguard.SSRFSafeClient(),
		sem:          make(chan struct{}, maxConcurrentDeliveries),
		// allowPrivateHosts stays false: the SSRF guard must be active.
	}

	// srv.URL is http://127.0.0.1:<port> - loopback, must be blocked pre-flight.
	d.deliver(0, "evil", srv.URL, "", events.NewEvent(events.EventType("test.event"), 1, nil))

	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("SSRF guard failed: loopback webhook server received %d request(s), want 0", got)
	}
}

// An outgoing delivery is signed with the DECRYPTED secret, so a receiver verifying
// with the plaintext the owner was given accepts it; a webhook whose stored secret
// cannot be decrypted is skipped rather than sent unsigned.
func TestDispatchSignsWithTheDecryptedSecret(t *testing.T) {
	enc, err := security.NewEncryptor(strings.Repeat("ef", 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := enc.SealOptional("whsec-out")
	if err != nil {
		t.Fatal(err)
	}

	type hit struct{ path, sig, body string }
	hits := make(chan hit, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits <- hit{r.URL.Path, r.Header.Get("X-Webhook-Signature"), string(b)}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr := db.NewProjectDBManager(t.TempDir())
	defer mgr.CloseAll()
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	for name, stored := range map[string]string{"signed": sealed, "broken": "not-ciphertext"} {
		res, err := pdb.ExecWrite(
			`INSERT INTO of_webhooks (name, url, secret_encrypted, is_enabled, direction) VALUES (?, ?, ?, 1, 'outgoing')`,
			name, srv.URL+"/"+name, stored)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := pdb.ExecWrite(`INSERT INTO of_webhook_subscriptions (webhook_id, event_type) VALUES (?, 'test.event')`, id); err != nil {
			t.Fatal(err)
		}
	}

	d := &Dispatcher{
		projectDBMgr:      mgr,
		encryptor:         enc,
		logger:            slog.New(slog.DiscardHandler),
		client:            &http.Client{Timeout: 5 * time.Second},
		sem:               make(chan struct{}, maxConcurrentDeliveries),
		allowPrivateHosts: true, // the fixture server is on loopback
	}
	d.handleEvent(events.Event{Type: events.EventType("test.event"), ProjectID: 1})

	select {
	case h := <-hits:
		if h.path != "/signed" {
			t.Fatalf("the first delivery went to %q, want /signed", h.path)
		}
		if want := "sha256=" + security.HMACSHA256Hex([]byte("whsec-out"), []byte(h.body)); h.sig != want {
			t.Errorf("signature %q, want %q (HMAC with the decrypted secret)", h.sig, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery arrived")
	}
	select {
	case h := <-hits:
		t.Errorf("a webhook with an undecryptable secret was delivered anyway: %+v", h)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSendRetriesAndSendsTheBodyAsIs pins the one outgoing sender a trigger's
// trigger_webhook action shares with event subscriptions: a 5xx is retried, the
// caller's body goes out byte for byte (no envelope added), and the outcome is logged.
func TestSendRetriesAndSendsTheBodyAsIs(t *testing.T) {
	var calls int64
	var lastBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		lastBody.Store(string(b))
		if atomic.AddInt64(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr := db.NewProjectDBManager(t.TempDir())
	defer mgr.CloseAll()
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	d := &Dispatcher{
		projectDBMgr:      mgr,
		logger:            slog.New(slog.DiscardHandler),
		client:            &http.Client{Timeout: 5 * time.Second},
		sem:               make(chan struct{}, maxConcurrentDeliveries),
		allowPrivateHosts: true, // the fixture server is on loopback
	}
	res, err := pdb.Writer().Exec(`INSERT INTO of_webhooks (name, url, direction) VALUES ('notify', ?, 'outgoing')`, srv.URL)
	if err != nil {
		t.Fatalf("seed webhook: %v", err)
	}
	id, _ := res.LastInsertId()
	body := []byte(`{"message":"Order placed"}`)
	if err := d.Send(Target{ID: int(id), Name: "notify", URL: srv.URL}, 1, "data.insert", body); err != nil {
		t.Fatalf("Send after one 500: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("receiver saw %d requests, want 2 (one retry)", got)
	}
	if got, _ := lastBody.Load().(string); got != string(body) {
		t.Fatalf("body = %q, want the caller's bytes unchanged", got)
	}
	var ok int
	if err := pdb.Reader().QueryRow(`SELECT COUNT(*) FROM of_webhook_logs WHERE success = 1`).Scan(&ok); err != nil || ok != 1 {
		t.Fatalf("successful delivery log rows = %d (err %v), want 1", ok, err)
	}
}
