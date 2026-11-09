package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSendOneParseErrorFallback covers the parse-error fallback: when Telegram rejects HTML entities
// (a tag split across chunks, or raw '<' in model output), sendOne retries once
// without parse_mode so the message lands as plaintext instead of vanishing.
func TestSendOneParseErrorFallback(t *testing.T) {
	var sawParseMode []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hasParse := strings.Contains(string(body), "parse_mode")
		sawParseMode = append(sawParseMode, hasParse)
		w.WriteHeader(200)
		if hasParse {
			w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities in message text"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer srv.Close()

	tp := &TelegramProvider{
		token:   "TESTTOKEN",
		baseURL: srv.URL,
		client:  &http.Client{Timeout: 5 * time.Second},
		logger:  slog.Default(),
	}

	id, err := tp.sendOne("123", "<unbalanced", nil)
	if err != nil {
		t.Fatalf("expected plaintext fallback to succeed, got %v", err)
	}
	if id != 7 {
		t.Errorf("message id = %d, want 7", id)
	}
	if len(sawParseMode) != 2 {
		t.Fatalf("expected 2 attempts (HTML then plaintext), got %d", len(sawParseMode))
	}
	if !sawParseMode[0] || sawParseMode[1] {
		t.Errorf("expected first attempt with parse_mode and retry without: %v", sawParseMode)
	}
}

// TestEditMessageKeyboardSendsMarkup verifies the in-place navigation edit
// actually carries reply_markup (so the buttons change, not just the text).
func TestEditMessageKeyboardSendsMarkup(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	defer srv.Close()

	tp := &TelegramProvider{token: "T", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, logger: slog.Default()}
	kb, err := buildInlineKeyboard([]string{"A"}, []string{"nav:menu"})
	if err != nil {
		t.Fatalf("build keyboard: %v", err)
	}
	if err := tp.EditMessageKeyboard("1", 9, "hi", kb); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("expected one call, got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], "reply_markup") || !strings.Contains(bodies[0], "inline_keyboard") {
		t.Errorf("reply_markup not sent: %s", bodies[0])
	}
}

// TestEditMessageKeyboardNotModified: an identical re-render (Telegram "message
// is not modified") is reported via isNotModifiedErr so callers treat it as a
// benign no-op instead of posting a duplicate.
func TestEditMessageKeyboardNotModified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":false,"description":"Bad Request: message is not modified"}`))
	}))
	defer srv.Close()

	tp := &TelegramProvider{token: "T", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, logger: slog.Default()}
	err := tp.EditMessageKeyboard("1", 9, "hi", nil)
	if !isNotModifiedErr(err) {
		t.Errorf("expected a not-modified error, got %v", err)
	}
}

// TestEditMessageKeyboardParseFallback: a malformed-HTML edit retries once
// without parse_mode so the screen still updates as plaintext.
func TestEditMessageKeyboardParseFallback(t *testing.T) {
	var sawParse []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hp := strings.Contains(string(body), "parse_mode")
		sawParse = append(sawParse, hp)
		w.WriteHeader(200)
		if hp {
			w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities in message text"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":9}}`))
	}))
	defer srv.Close()

	tp := &TelegramProvider{token: "T", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, logger: slog.Default()}
	if err := tp.EditMessageKeyboard("1", 9, "<bad", nil); err != nil {
		t.Fatalf("expected plaintext fallback to succeed, got %v", err)
	}
	if len(sawParse) != 2 || !sawParse[0] || sawParse[1] {
		t.Errorf("expected HTML then plaintext retry, got %v", sawParse)
	}
}

// TestApiCallRedactsTokenOnHTTPError covers token redaction: a transport error must never
// carry the bot token (Go's *url.Error embeds the full request URL).
func TestApiCallRedactsTokenOnHTTPError(t *testing.T) {
	const token = "1234567890:AAH-SECRET-TOKEN"
	tp := &TelegramProvider{
		token:   token,
		baseURL: "http://127.0.0.1:1/bot" + token, // unroutable: forces a transport error
		client:  &http.Client{Timeout: 500 * time.Millisecond},
		logger:  slog.Default(),
	}
	_, err := tp.apiCall("getMe", map[string]any{})
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("bot token leaked in error: %q", err.Error())
	}
}

// TestApiCallCtxParsesRetryAfter: a 429 must surface as a *tgAPIError carrying
// the status and Telegram's requested retry_after, so callers can back off by
// the server's request instead of dropping the message.
func TestApiCallCtxParsesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 5","parameters":{"retry_after":5}}`))
	}))
	defer srv.Close()

	tp := &TelegramProvider{token: "T", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, logger: slog.Default()}
	_, err := tp.apiCallCtx(context.Background(), "sendMessage", map[string]any{"chat_id": "1", "text": "hi"})
	var apiErr *tgAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *tgAPIError, got %T: %v", err, err)
	}
	if apiErr.status != 429 {
		t.Errorf("status = %d, want 429", apiErr.status)
	}
	if apiErr.retryAfter != 5*time.Second {
		t.Errorf("retryAfter = %v, want 5s", apiErr.retryAfter)
	}
	if !isRetryable(apiErr.status) {
		t.Error("429 should be retryable")
	}
}

// TestSendRetriesOn429: an outbound send that first hits 429 is retried rather
// than dropped, and ultimately succeeds. (No retry_after in the body, so the
// wrapper uses its short default backoff, keeping the test fast.)
func TestSendRetriesOn429(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(429)
			w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests"}`))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true,"result":{"message_id":11}}`))
	}))
	defer srv.Close()

	tp := &TelegramProvider{token: "T", baseURL: srv.URL, client: &http.Client{Timeout: 5 * time.Second}, logger: slog.Default()}
	id, err := tp.sendOne("1", "hi", nil)
	if err != nil {
		t.Fatalf("expected the retry to succeed, got %v", err)
	}
	if id != 11 {
		t.Errorf("message id = %d, want 11", id)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (429 then success)", attempts)
	}
}

// TestDispatchAnswersCallbackWithNilMessage: a callback_query whose originating
// message Telegram dropped (>~48h old, so Message is nil) must still be answered
// so the button's spinner clears; the routing handler must not run.
func TestDispatchAnswersCallbackWithNilMessage(t *testing.T) {
	var answered bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			answered = true
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	var onCallbackCalled bool
	tp := &TelegramProvider{
		token:       "T",
		baseURL:     srv.URL,
		client:      &http.Client{Timeout: 5 * time.Second},
		logger:      slog.Default(),
		warnedChats: map[int64]bool{},
		onCallback: func(_, _, _ string, _ int, _ string) string {
			onCallbackCalled = true
			return ""
		},
	}
	tp.dispatch(tgUpdate{CallbackQuery: &tgCallbackQuery{ID: "cb1", Data: "x", Message: nil}})
	if !answered {
		t.Error("expected answerCallbackQuery to be called so the spinner clears")
	}
	if onCallbackCalled {
		t.Error("onCallback must not run for a callback with no message")
	}
}

// TestProcessUpdatesSavesOffsetAfterDispatch pins the ordering that makes
// delivery at-least-once: the update is dispatched first, then the offset is
// persisted, so a crash in between replays rather than drops the update.
func TestProcessUpdatesSavesOffsetAfterDispatch(t *testing.T) {
	var order []string
	tp := &TelegramProvider{
		token:       "T",
		logger:      slog.Default(),
		warnedChats: map[int64]bool{},
		offsets:     &recordingOffsets{order: &order},
		onMessage: func(_, _, _ string, _ int, _ string) {
			order = append(order, "dispatch")
		},
	}
	updates := []tgUpdate{{
		UpdateID: 100,
		Message:  &tgMessage{MessageID: 5, Text: "hi", Chat: tgChat{ID: 42, Type: "private"}, From: &tgUser{ID: 42}},
	}}
	next := tp.processUpdates(updates, 0)
	if next != 101 {
		t.Errorf("next offset = %d, want 101", next)
	}
	if len(order) != 2 || order[0] != "dispatch" || order[1] != "save" {
		t.Fatalf("expected [dispatch save], got %v", order)
	}
}

// recordingOffsets is an OffsetStore that logs each SaveOffset into a shared
// slice so a test can assert dispatch-then-save ordering.
type recordingOffsets struct{ order *[]string }

func (r *recordingOffsets) LoadOffset() int  { return 0 }
func (r *recordingOffsets) SaveOffset(o int) { *r.order = append(*r.order, "save") }
