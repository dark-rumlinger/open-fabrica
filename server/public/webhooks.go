package public

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/events"
	"openfabrica/security"
)

// webhookTimestampHeader opts a sender into replay protection. When present, the
// signature must cover "<timestamp>.<body>" and the timestamp (unix seconds) must
// be within webhookTimestampWindow of now, so a captured request stops working
// after a few minutes. Senders that cannot add a header keep the body-only
// signature and stay replayable (see docs/SECURITY.md).
const (
	webhookTimestampHeader = "X-Webhook-Timestamp"
	webhookTimestampWindow = 5 * time.Minute
)

// webhookSignedPayload returns the bytes the signature must cover: the body
// itself, or "<timestamp>.<body>" when the sender supplied a timestamp header.
// It errors when the timestamp is not a unix-seconds integer or falls outside the
// replay window.
func webhookSignedPayload(body []byte, tsHeader string, now time.Time) ([]byte, error) {
	tsHeader = strings.TrimSpace(tsHeader)
	if tsHeader == "" {
		return body, nil
	}
	sec, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("timestamp is not unix seconds")
	}
	if age := now.Sub(time.Unix(sec, 0)); age > webhookTimestampWindow || age < -webhookTimestampWindow {
		return nil, fmt.Errorf("timestamp is outside the %s window", webhookTimestampWindow)
	}
	return append([]byte(tsHeader+"."), body...), nil
}

// IncomingWebhook handles POST /webhooks/{name} for receiving external webhook payloads.
// Validates HMAC-SHA256 signature from X-Webhook-Signature header.
func (h *Handler) IncomingWebhook(w http.ResponseWriter, r *http.Request) {
	project := getProject(r)
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	webhookName := chi.URLParam(r, "name")
	if webhookName == "" {
		writeError(w, http.StatusNotFound, "webhook name required")
		return
	}

	projectDB := h.openProjectDB(w, project.ID)
	if projectDB == nil {
		return
	}

	// Look up the webhook.
	var webhookID int
	var sealed string
	var isEnabled bool
	err := projectDB.QueryRow(
		"SELECT id, secret_encrypted, is_enabled FROM of_webhooks WHERE name = ? AND COALESCE(direction, 'incoming') = 'incoming'",
		webhookName,
	).Scan(&webhookID, &sealed, &isEnabled)
	if err != nil {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	secret, err := h.deps.Encryptor.OpenOptional(sealed)
	if err != nil {
		// A secret that cannot be decrypted (the key was replaced) cannot verify any
		// signature, so nothing is accepted rather than anything unverified.
		h.deps.Logger.Error("incoming webhook secret cannot be decrypted",
			"webhook", webhookName, "project_id", project.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "webhook signing secret is unavailable")
		return
	}

	if !isEnabled {
		writeError(w, http.StatusForbidden, "webhook is disabled")
		return
	}

	// Read the body.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	// Every incoming webhook fires server-side side effects (engine context,
	// triggers, emails, data writes), so it must be authenticated. A webhook
	// with no signing secret cannot be verified and is rejected outright -
	// accepting it would let anyone who knows the (guessable) webhook name forge
	// trusted events. Webhooks are always created with a secret; an empty one
	// here means an unsecured row that must be re-secured before use.
	if secret == "" {
		h.deps.Logger.Warn("incoming webhook rejected: no signing secret configured",
			"webhook", webhookName, "project_id", project.ID, "remote_addr", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "webhook has no signing secret configured")
		return
	}

	// Validate HMAC-SHA256 signature. A missing signature is rejected, not
	// treated as an unsigned delivery.
	signature := r.Header.Get("X-Webhook-Signature")
	if signature == "" {
		signature = r.Header.Get("X-Hub-Signature-256") // GitHub webhook format support
	}
	if signature == "" {
		h.deps.Logger.Warn("webhook missing required signature",
			"webhook", webhookName, "project_id", project.ID, "remote_addr", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "missing signature")
		return
	}
	// Strip "sha256=" prefix if present (GitHub format).
	sig := strings.TrimPrefix(signature, "sha256=")
	// Constant-time compare of the lowercase hex encodings. Lowercasing the
	// provided signature keeps uppercase-hex signers accepted, exactly as the
	// old decode-then-compare did; a malformed (non-hex) signature can never
	// equal the valid hex on the right, so it is rejected the same way.
	signed, tsErr := webhookSignedPayload(body, r.Header.Get(webhookTimestampHeader), time.Now())
	if tsErr != nil {
		h.deps.Logger.Warn("webhook timestamp rejected",
			"webhook", webhookName, "project_id", project.ID, "remote_addr", r.RemoteAddr, "reason", tsErr.Error())
		writeError(w, http.StatusUnauthorized, "stale or invalid timestamp")
		return
	}
	expectedMAC := security.HMACSHA256Hex([]byte(secret), signed)
	if !hmac.Equal([]byte(strings.ToLower(sig)), []byte(expectedMAC)) {
		h.deps.Logger.Warn("webhook signature validation failed",
			"webhook", webhookName, "project_id", project.ID, "remote_addr", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// Update last_triggered. Bookkeeping only; a verified webhook still fires.
	if _, err := projectDB.ExecWrite(
		"UPDATE of_webhooks SET last_triggered = CURRENT_TIMESTAMP WHERE id = ?",
		webhookID,
	); err != nil {
		h.deps.Logger.Warn("webhook last_triggered update failed",
			"webhook", webhookName, "project_id", project.ID, "error", err)
	}

	// Log the webhook receipt.
	if _, err := projectDB.ExecWrite(
		"INSERT INTO of_webhook_logs (webhook_id, direction, event_type, payload, success) VALUES (?, 'incoming', 'webhook.received', ?, 1)",
		webhookID, string(body),
	); err != nil {
		h.deps.Logger.Warn("webhook receipt log failed",
			"webhook", webhookName, "project_id", project.ID, "error", err)
	}

	// Publish event so the engine can react.
	if h.deps.Bus != nil {
		h.deps.Bus.Publish(events.NewEvent(events.EventWebhookReceived, project.ID,
			webhookEventPayload(webhookID, webhookName, body)))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "webhook received",
	})
}

// webhookEventPayload builds the webhook.received event payload. The parsed body
// fields are flattened to the top level so a trigger reads them directly with
// {{field}} (matching the trigger-events contract and the flat convention of
// every other event); a non-JSON body simply contributes no fields.
// webhook_id/webhook_name are platform metadata written LAST, so a body field of
// the same name can never shadow or spoof them.
func webhookEventPayload(webhookID int, webhookName string, body []byte) map[string]any {
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	payload := make(map[string]any, len(parsed)+2)
	for k, v := range parsed {
		payload[k] = v
	}
	payload["webhook_id"] = webhookID
	payload["webhook_name"] = webhookName
	return payload
}
