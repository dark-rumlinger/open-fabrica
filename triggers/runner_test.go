package triggers

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/events"
)

// TestHandleEventBoundsConcurrency verifies the trigger fan-out semaphore:
// firing an event matched by far more triggers than maxConcurrentActions must
// still execute every trigger while never exceeding the in-flight cap. A
// regression to a `for { go execute }` would push observed concurrency
// up to the trigger count. http_request is used as the action because it does
// observable work (an outbound HTTP call) through the runner's client.
func TestHandleEventBoundsConcurrency(t *testing.T) {
	const triggers = 40

	var inFlight, maxInFlight, executed int64
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
		atomic.AddInt64(&executed, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr := db.NewProjectDBManager(t.TempDir())
	defer mgr.CloseAll()
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	for i := 0; i < triggers; i++ {
		if _, err := pdb.ExecWrite(
			`INSERT INTO of_triggers (name, event_type, trigger_type, trigger_config, is_enabled)
			 VALUES (?, 'test.event', 'http_request', ?, 1)`,
			"trig-"+string(rune('A'+i%26))+string(rune('0'+i/26)),
			`{"url":"`+srv.URL+`"}`,
		); err != nil {
			t.Fatalf("seed trigger %d: %v", i, err)
		}
	}

	r := &Runner{
		projectDBMgr: mgr,
		logger:       slog.New(slog.DiscardHandler),
		client:       &http.Client{Timeout: 5 * time.Second},
		sem:          make(chan struct{}, maxConcurrentActions),
		// The fixture server is on loopback, which the SSRF guard blocks by
		// default; allow it so this test exercises the concurrency semaphore.
		allowPrivateHosts: true,
	}

	r.handleEvent(events.Event{Type: events.EventType("test.event"), ProjectID: 1})

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&executed) < triggers {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: executed %d/%d", atomic.LoadInt64(&executed), triggers)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := atomic.LoadInt64(&maxInFlight); got > maxConcurrentActions {
		t.Errorf("in-flight actions reached %d, exceeds cap of %d", got, maxConcurrentActions)
	}
	if got := atomic.LoadInt64(&maxInFlight); got < 2 {
		t.Errorf("expected overlapping actions (saturated cap), only saw %d in flight", got)
	}
}
