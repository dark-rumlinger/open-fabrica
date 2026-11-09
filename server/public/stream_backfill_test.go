package public

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/events"
)

func TestParseLastEventID(t *testing.T) {
	mk := func(header, query string) *http.Request {
		r := httptest.NewRequest("GET", "/api/x/stream?"+query, nil)
		if header != "" {
			r.Header.Set("Last-Event-ID", header)
		}
		return r
	}
	cases := []struct {
		name          string
		header, query string
		want          uint64
	}{
		{"header", "17", "", 17},
		{"query fallback", "", "lastEventId=9", 9},
		{"header wins over query", "5", "lastEventId=9", 5},
		{"absent", "", "", 0},
		{"garbage", "abc", "", 0},
		{"negative", "-1", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLastEventID(mk(tc.header, tc.query)); got != tc.want {
				t.Errorf("parseLastEventID = %d, want %d", got, tc.want)
			}
		})
	}
}

// syncFlushRec is a concurrency-safe ResponseWriter+Flusher: apiStream writes from
// its own goroutine while the test reads the accumulated body.
type syncFlushRec struct {
	mu  sync.Mutex
	buf bytes.Buffer
	hdr http.Header
}

func (s *syncFlushRec) Header() http.Header { return s.hdr }
func (s *syncFlushRec) WriteHeader(int)     {}
func (s *syncFlushRec) Flush()              {}
func (s *syncFlushRec) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncFlushRec) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A client that drops and reconnects sends the id of the last event it saw as
// Last-Event-ID. The stream must replay the retained events AFTER it (so the gap is
// not silently lost) and NOT the one it already has.
func TestApiStreamBackfillReplaysMissedEvents(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	t.Cleanup(mgr.CloseAll)
	if _, err := pdb.Writer().Exec(
		`INSERT INTO of_stream_endpoints (path, event_types, requires_auth) VALUES ('notes', '["data.insert"]', 0)`); err != nil {
		t.Fatalf("register stream endpoint: %v", err)
	}

	bus := events.NewBus()
	h := &Handler{deps: &Deps{Bus: bus}}

	// Publish three events BEFORE anyone connects; they land in the backfill buffer.
	// No "endpoint" field, so the row-level scope check treats them as out of scope
	// and passes them (see streamScope.allow).
	for i := 1; i <= 3; i++ {
		bus.Publish(events.Event{ProjectID: 1, Type: "data.insert", Payload: map[string]any{"table": "notes", "id": i}})
	}
	buffered := bus.Since(0)
	if len(buffered) != 3 {
		t.Fatalf("want 3 buffered events, got %d", len(buffered))
	}
	firstID := buffered[0].ID // the event the client claims to already have

	rec := &syncFlushRec{hdr: http.Header{}}
	req := httptest.NewRequest("GET", "/api/notes/stream", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(firstID, 10))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() { h.apiStream(rec, req, 1, pdb.Reader()); close(done) }()

	// Backfill is written synchronously before the live select loop, so the two
	// replayed frames appear promptly; then stop the handler.
	deadline := time.Now().Add(3 * time.Second)
	for strings.Count(rec.body(), "event: data.insert") < 2 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("backfill did not replay the two missed events; body:\n%s", rec.body())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	body := rec.body()
	if strings.Contains(body, fmt.Sprintf("id: %d\n", firstID)) {
		t.Errorf("backfill must NOT replay the already-seen event (id %d):\n%s", firstID, body)
	}
	for _, e := range buffered[1:] {
		if !strings.Contains(body, fmt.Sprintf("id: %d\n", e.ID)) {
			t.Errorf("backfill missing missed event id %d:\n%s", e.ID, body)
		}
	}
	if n := strings.Count(body, "event: data.insert"); n != 2 {
		t.Errorf("expected exactly 2 replayed events, got %d:\n%s", n, body)
	}
}
