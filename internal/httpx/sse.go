package httpx

import (
	"fmt"
	"net/http"
	"time"
)

// SSEWriter emits Server-Sent Events frames on a prepared response. It is the
// one browser-facing SSE producer (the consumer side - parsing an upstream
// provider's SSE - lives in providers/llm and is deliberately separate).
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSE prepares w for a Server-Sent-Events response: acquires the flusher
// (ok=false when the writer can't stream - the caller answers the error),
// clears the write deadline for the long-lived connection, and sets the SSE
// headers. Call before writing anything to w.
func NewSSE(w http.ResponseWriter) (*SSEWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // best-effort: not all writers support deadlines
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	return &SSEWriter{w: w, flusher: flusher}, true
}

// Event writes and flushes one `event: <name>\ndata: <data>\n\n` frame.
// Returns the write error so a broken client ends the caller's loop.
func (s *SSEWriter) Event(name string, data []byte) error {
	return s.EventID(0, name, data)
}

// EventID is Event with an `id:` line, which the browser's EventSource records
// and echoes as the Last-Event-ID header on reconnect so the producer can
// backfill from it. An id of 0 is omitted: SSE has no "unset" id, so writing 0
// would clobber the client's last real id with a useless one.
func (s *SSEWriter) EventID(id uint64, name string, data []byte) error {
	var err error
	if id > 0 {
		_, err = fmt.Fprintf(s.w, "id: %d\nevent: %s\ndata: %s\n\n", id, name, data)
	} else {
		_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data)
	}
	s.flusher.Flush()
	return err
}

// Comment writes and flushes a comment frame (`: <text>\n\n`) - the keepalive
// shape, invisible to EventSource consumers.
func (s *SSEWriter) Comment(text string) error {
	_, err := fmt.Fprintf(s.w, ": %s\n\n", text)
	s.flusher.Flush()
	return err
}
