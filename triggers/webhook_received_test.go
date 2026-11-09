package triggers

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/events"
)

// TestWebhookReceivedTriggerReadsBodyField pins the webhook.received contract:
// the parsed webhook body is flattened to the event payload top level (see
// public.webhookEventPayload), so a trigger resolves a body field with
// {{field}}. Platform metadata {{webhook_name}} must still resolve alongside it.
// A regression to nesting the body under a `payload` key would leave {{email}}
// unresolved and this test's forwarded value empty.
func TestWebhookReceivedTriggerReadsBodyField(t *testing.T) {
	captured := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]string
		_ = json.Unmarshal(body, &got)
		captured <- got
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr := db.NewProjectDBManager(t.TempDir())
	defer mgr.CloseAll()
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}

	// A trigger that forwards a webhook BODY field ({{email}}) and platform
	// METADATA ({{webhook_name}}) out via http_request, so both resolutions are
	// observable in the received request body.
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_triggers (name, event_type, trigger_type, trigger_config, is_enabled)
		 VALUES ('forward', 'webhook.received', 'http_request', ?, 1)`,
		`{"url":"`+srv.URL+`","method":"POST","body":{"email":"{{email}}","source":"{{webhook_name}}"}}`,
	); err != nil {
		t.Fatalf("seed trigger: %v", err)
	}

	r := &Runner{
		projectDBMgr: mgr,
		logger:       slog.New(slog.DiscardHandler),
		client:       &http.Client{Timeout: 5 * time.Second},
		sem:          make(chan struct{}, maxConcurrentActions),
		// The fixture server is on loopback, which the SSRF guard blocks by
		// default; allow it so the resolved request actually reaches the server.
		allowPrivateHosts: true,
	}

	// Payload shaped exactly as public.webhookEventPayload produces it: the body's
	// fields flattened to the top level, plus platform metadata.
	r.handleEvent(events.Event{
		Type:      events.EventWebhookReceived,
		ProjectID: 1,
		Payload: map[string]any{
			"email":        "alice@example.com",
			"webhook_id":   7,
			"webhook_name": "stripe",
		},
	})

	select {
	case got := <-captured:
		if got["email"] != "alice@example.com" {
			t.Errorf("body field {{email}} resolved to %q, want %q", got["email"], "alice@example.com")
		}
		if got["source"] != "stripe" {
			t.Errorf("metadata {{webhook_name}} resolved to %q, want %q", got["source"], "stripe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the trigger's http_request")
	}
}
