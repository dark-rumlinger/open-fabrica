package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"openfabrica/internal/msgcat"
	"openfabrica/internal/replylang"
)

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out.Error
}

func serve(t *testing.T, locale, msg string) string {
	t.Helper()
	h := msgcat.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, http.StatusNotFound, msg)
	}))
	req := httptest.NewRequest("GET", "/x", nil)
	if locale != "" {
		req.Header.Set(replylang.Header, locale)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	return errorBody(t, rec)
}

// An error written through WriteError reaches the caller in their language, with
// no change to the handler that wrote it.
func TestWriteErrorSpeaksTheCallersLanguage(t *testing.T) {
	want := msgcat.Catalog("es")["project not found"]
	if want == "" {
		t.Fatal("the Spanish catalog lacks 'project not found'")
	}
	if got := serve(t, "es", "project not found"); got != want {
		t.Errorf("es: got %q, want %q", got, want)
	}
	if got := serve(t, "", "project not found"); got != "project not found" {
		t.Errorf("no locale: got %q", got)
	}
	if got := serve(t, "en", "project not found"); got != "project not found" {
		t.Errorf("en: got %q", got)
	}
	// A message nobody wrote down passes through, whatever the language.
	if got := serve(t, "es", "disk is on fire"); got != "disk is on fire" {
		t.Errorf("unknown message: got %q", got)
	}
}

func TestWriteErrorTranslatesTemplatedMessages(t *testing.T) {
	got := serve(t, "de", "failed to create project database: disk full")
	want := "Projektdatenbank konnte nicht erstellt werden: disk full"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Without the middleware (the public server) nothing changes.
func TestWriteErrorWithoutMiddlewareIsEnglish(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusBadRequest, "project not found")
	if got := errorBody(t, rec); got != "project not found" {
		t.Errorf("got %q", got)
	}
}
