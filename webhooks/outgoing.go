package webhooks

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"openfabrica/db"
	"openfabrica/events"
	"openfabrica/internal/netguard"
	"openfabrica/internal/safego"
	"openfabrica/security"
)

// maxConcurrentDeliveries caps the number of in-flight webhook HTTP deliveries
// across all projects. Without a ceiling a single hot event with many subscribed
// targets fans out into hundreds of retrying HTTP calls with no backpressure.
const maxConcurrentDeliveries = 16

// Dispatcher delivers outgoing webhook payloads when subscribed events fire.
type Dispatcher struct {
	projectDBMgr *db.ProjectDBManager
	bus          *events.Bus
	// encryptor opens each webhook's stored signing secret.
	encryptor *security.Encryptor
	logger    *slog.Logger
	client    *http.Client
	sem       chan struct{}
	// ctx is the dispatcher's lifecycle context: retry backoff waits and
	// in-flight requests must abort when it is canceled at shutdown. Nil on a
	// zero-value Dispatcher (tests).
	ctx context.Context
	// allowPrivateHosts disables the SSRF pre-flight so delivery to a
	// loopback/private address is permitted. Off in production (a webhook must
	// not be able to probe the loopback admin plane or cloud metadata); set only
	// in tests that deliver to a local httptest server.
	allowPrivateHosts bool
}

// NewDispatcher creates an outgoing webhook dispatcher and subscribes to all
// events. ctx is the process-lifecycle context; canceling it aborts in-flight
// deliveries and their retry backoffs.
func NewDispatcher(projectDBMgr *db.ProjectDBManager, bus *events.Bus, encryptor *security.Encryptor, ctx context.Context) *Dispatcher {
	// SSRF-hardened: webhook URLs are stored config (set by the AI/admin), so the
	// client re-validates redirects and blocks private/internal IPs at dial time.
	// Without this a webhook pointed at 127.0.0.1:<admin> or 169.254.169.254 would
	// be fetched server-side and its response logged. URLs are also pre-validated
	// in deliver().
	client := netguard.SSRFSafeClient()
	client.Timeout = 15 * time.Second
	d := &Dispatcher{
		projectDBMgr: projectDBMgr,
		bus:          bus,
		encryptor:    encryptor,
		logger:       slog.With("component", "webhook_dispatcher"),
		client:       client,
		sem:          make(chan struct{}, maxConcurrentDeliveries),
		ctx:          ctx,
	}

	// Subscribe to all events and check for matching webhook subscriptions.
	bus.SubscribeAll(d.handleEvent)

	return d
}

// handleEvent checks if any outgoing webhooks are subscribed to this event type.
func (d *Dispatcher) handleEvent(event events.Event) {
	if event.ProjectID == 0 {
		return
	}

	eventType := string(event.Type)

	// Get the project DB for this event's project.
	projectDB := d.projectDBMgr.Get(event.ProjectID)
	if projectDB == nil {
		return // project DB not open, skip
	}

	// Query for outgoing webhooks subscribed to this event type.
	rows, err := projectDB.Query(
		`SELECT w.id, w.name, w.url, w.secret_encrypted
		 FROM of_webhooks w
		 JOIN of_webhook_subscriptions ws ON ws.webhook_id = w.id
		 WHERE ws.event_type = ? AND w.is_enabled = 1 AND w.url IS NOT NULL AND w.url != ''`,
		eventType,
	)
	if err != nil {
		slog.Warn("webhook dispatch: subscription lookup failed", "event", eventType, "error", err)
		return
	}
	defer rows.Close()

	type target struct {
		id     int
		name   string
		url    string
		secret string
	}

	var targets []target
	for rows.Next() {
		var t target
		var sealed sql.NullString
		if err := rows.Scan(&t.id, &t.name, &t.url, &sealed); err != nil {
			continue
		}
		if sealed.Valid {
			secret, err := d.encryptor.OpenOptional(sealed.String)
			if err != nil {
				// Delivering unsigned would look like a working webhook to a
				// receiver that does not check, and a failing one to one that does:
				// skip it and say why instead.
				slog.Warn("webhook dispatch: signing secret cannot be decrypted, skipping",
					"webhook", t.name, "error", err)
				continue
			}
			t.secret = secret
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("webhook dispatch: subscription read failed", "event", eventType, "error", err)
	}
	// Fan out in a single detached goroutine so the event bus (which calls
	// subscribers synchronously, in the publisher's goroutine) is never blocked.
	// That goroutine drains the targets through the semaphore, so in-flight HTTP
	// deliveries stay globally capped at maxConcurrentDeliveries without applying
	// backpressure to Publish.
	safego.Go("webhook fan-out", func() {
		for _, t := range targets {
			d.sem <- struct{}{}
			go func(t target) {
				defer func() { <-d.sem }()
				// Recover so a panic in one delivery cannot crash the whole
				// multi-project server from this detached goroutine. The webhook
				// name rides in the task label.
				defer safego.Recover("webhook delivery " + t.name)
				d.deliver(t.id, t.name, t.url, t.secret, event)
			}(t)
		}
	})
}

// deliver sends one subscribed event to a webhook, wrapped in the subscription
// envelope {event, project_id, payload, timestamp}.
func (d *Dispatcher) deliver(webhookID int, name, url, secret string, event events.Event) {
	payload, err := json.Marshal(map[string]any{
		"event":      string(event.Type),
		"project_id": event.ProjectID,
		"payload":    event.Payload,
		"timestamp":  event.Timestamp,
	})
	if err != nil {
		d.logger.Error("webhook: failed to marshal payload", "webhook", name, "error", err)
		return
	}
	_ = d.Send(Target{ID: webhookID, Name: name, URL: url, Secret: secret}, event.ProjectID, string(event.Type), payload)
}

// Target is one outgoing webhook: its row id, name, URL and opened signing secret.
type Target struct {
	ID     int
	Name   string
	URL    string
	Secret string
}

// Send delivers body to t, signed with HMAC-SHA256 when t has a secret, with the
// SSRF pre-flight, up to 3 attempts with backoff, a row in of_webhook_logs, and
// last_triggered on success. It is the one outgoing-webhook sender: event
// subscriptions and a trigger's trigger_webhook action both go through it, and only
// the body differs. Returns the final error, nil on a 2xx.
func (d *Dispatcher) Send(t Target, projectID int, eventType string, body []byte) error {
	// Pre-flight SSRF check: refuse a stored URL that resolves to a private or
	// internal address before spending a delivery attempt on it.
	if !d.allowPrivateHosts {
		if err := netguard.ValidateExternalURL(t.URL); err != nil {
			d.logDelivery(t.ID, projectID, eventType, string(body), 0, "", false)
			d.logger.Error("webhook delivery blocked: unsafe url", "webhook", t.Name, "url", t.URL, "error", err)
			return fmt.Errorf("unsafe url: %w", err)
		}
	}

	// Only network errors, 5xx, and 429 are retried; any other 4xx is a permanent
	// rejection of this payload, so a retry cannot change the outcome.
	var lastErr error
	var statusCode int
	var responseBody string

	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background() // zero-value Dispatcher (tests) has no lifecycle ctx
	}

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second): // 2s, 8s
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "OpenFabrica-Webhook/1.0")
		if t.Secret != "" {
			req.Header.Set("X-Webhook-Signature", "sha256="+security.HMACSHA256Hex([]byte(t.Secret), body))
		}

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		statusCode = resp.StatusCode
		responseBody = string(respBody)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			d.logDelivery(t.ID, projectID, eventType, string(body), statusCode, responseBody, true)
			if sdb := d.projectDBMgr.Get(projectID); sdb != nil {
				_, _ = sdb.ExecWrite("UPDATE of_webhooks SET last_triggered = CURRENT_TIMESTAMP WHERE id = ?", t.ID) // best-effort bookkeeping
			}
			d.logger.Info("webhook delivered", "webhook", t.Name, "url", t.URL, "status", statusCode)
			return nil
		}

		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			break // non-retryable client error
		}
	}

	// Retries exhausted, or a non-retryable status.
	d.logDelivery(t.ID, projectID, eventType, string(body), statusCode, responseBody, false)
	d.logger.Error("webhook delivery failed", "webhook", t.Name, "url", t.URL, "error", lastErr)
	return lastErr
}

func (d *Dispatcher) logDelivery(webhookID, projectID int, eventType, payload string, statusCode int, response string, success bool) {
	if sdb := d.projectDBMgr.Get(projectID); sdb != nil {
		if _, err := sdb.ExecWrite(
			"INSERT INTO of_webhook_logs (webhook_id, direction, event_type, payload, status_code, response, success) VALUES (?, 'outgoing', ?, ?, ?, ?, ?)",
			webhookID, eventType, payload, statusCode, response, success,
		); err != nil {
			d.logger.Warn("webhook: failed to record delivery log", "webhook_id", webhookID, "error", err)
		}
	}
}
