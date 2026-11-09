package httpx

import (
	"net/http/httptest"
	"testing"
)

// EventID emits an `id:` line so the browser records it and echoes it as
// Last-Event-ID on reconnect; Event (id 0) omits it, since writing 0 would clobber
// the client's last real id.
func TestSSEWriterEventID(t *testing.T) {
	rec := httptest.NewRecorder()
	s, ok := NewSSE(rec)
	if !ok {
		t.Fatal("NewSSE: httptest recorder must support flushing")
	}
	if err := s.EventID(42, "data.insert", []byte(`{"x":1}`)); err != nil {
		t.Fatalf("EventID: %v", err)
	}
	if err := s.Event("data.update", []byte(`{"y":2}`)); err != nil {
		t.Fatalf("Event: %v", err)
	}
	got := rec.Body.String()
	want := "id: 42\nevent: data.insert\ndata: {\"x\":1}\n\n" +
		"event: data.update\ndata: {\"y\":2}\n\n"
	if got != want {
		t.Errorf("SSE frames =\n%q\nwant\n%q", got, want)
	}
}
