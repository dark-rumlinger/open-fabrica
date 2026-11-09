package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"openfabrica/internal/netguard"
	"openfabrica/tools"
)

// webhookRouteName is the shape an incoming webhook name must have to be a single,
// routable path segment.
var webhookRouteName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func webhookCreate(ctx *tools.ToolContext, name, url string) (*tools.Result, error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("generating secret: %w", err)
	}
	secret := hex.EncodeToString(secretBytes)

	direction := "incoming"
	if url != "" {
		direction = "outgoing"
	}
	// An incoming webhook is addressed by name in the URL path, so a name with a
	// slash or a space would create a route no sender can reach.
	if direction == "incoming" && !webhookRouteName.MatchString(name) {
		return tools.FailCode(tools.CodeBadArg, "webhook name %q is used in the URL path /webhooks/<name>; use 1-64 letters, digits, '_' or '-'", name), nil
	}

	// Stored encrypted; the plaintext is returned once below so the owner can
	// configure the sender, and read back through webhookGet when needed.
	sealed, err := ctx.Encryptor.SealOptional(secret)
	if err != nil {
		return nil, fmt.Errorf("encrypting webhook secret: %w", err)
	}
	var exists int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_webhooks WHERE name = ?", name).Scan(&exists)
	if exists > 0 {
		return tools.FailCode("conflict", "a webhook named %q already exists - pick another name, or change it with manage_webhooks action=update", name), nil
	}
	result, err := ctx.DB.Exec(
		"INSERT INTO of_webhooks (name, secret_encrypted, url, direction) VALUES (?, ?, ?, ?)",
		name, sealed, url, direction,
	)
	if err != nil {
		return nil, fmt.Errorf("creating webhook: %w", err)
	}

	id, _ := result.LastInsertId()
	data := map[string]any{
		"id":        id,
		"name":      name,
		"secret":    secret,
		"direction": direction,
	}
	if url != "" {
		data["url"] = url
	} else {
		// The route the sender posts to; a webhook.received trigger reacts to it.
		data["route"] = "POST /webhooks/" + name
	}
	return tools.Ok(data), nil
}

func webhookGet(ctx *tools.ToolContext, name string) (*tools.Result, error) {
	var id int
	var sealed, direction string
	var url sql.NullString
	var isEnabled bool
	var lastTriggered sql.NullTime
	var createdAt time.Time

	err := ctx.DB.QueryRow(
		"SELECT id, secret_encrypted, url, direction, is_enabled, last_triggered, created_at FROM of_webhooks WHERE name = ?",
		name,
	).Scan(&id, &sealed, &url, &direction, &isEnabled, &lastTriggered, &createdAt)
	if err == sql.ErrNoRows {
		return tools.NotFound("webhook", name), nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying webhook: %w", err)
	}
	secret, err := ctx.Encryptor.OpenOptional(sealed)
	if err != nil {
		return tools.Fail("the webhook's signing secret cannot be decrypted (was the encryption key replaced?): %v", err), nil
	}

	wh := map[string]any{
		"id":         id,
		"name":       name,
		"secret":     secret,
		"direction":  direction,
		"is_enabled": isEnabled,
		"created_at": createdAt,
	}
	if url.Valid && url.String != "" {
		wh["url"] = url.String
	}
	if lastTriggered.Valid {
		wh["last_triggered"] = lastTriggered.Time
	}

	rows, err := ctx.DB.Query(
		"SELECT event_type FROM of_webhook_subscriptions WHERE webhook_id = ? ORDER BY event_type",
		id,
	)
	if err == nil {
		defer rows.Close()
		var subs []string
		for rows.Next() {
			var et string
			if rows.Scan(&et) == nil {
				subs = append(subs, et)
			}
		}
		_ = rows.Err() // advisory extra; tolerated like the Query error
		if len(subs) > 0 {
			wh["subscriptions"] = subs
		}
	}

	return tools.Ok(wh), nil
}

func webhookList(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT id, name, direction, url, is_enabled, last_triggered, created_at FROM of_webhooks ORDER BY name",
	)
	if err != nil {
		return nil, fmt.Errorf("listing of_webhooks: %w", err)
	}
	defer rows.Close()

	var webhooks []map[string]any
	for rows.Next() {
		var id int
		var name, direction string
		var url sql.NullString
		var isEnabled bool
		var lastTriggered sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &direction, &url, &isEnabled, &lastTriggered, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning webhook: %w", err)
		}
		wh := map[string]any{
			"id":         id,
			"name":       name,
			"direction":  direction,
			"is_enabled": isEnabled,
			"created_at": createdAt,
		}
		if url.Valid && url.String != "" {
			wh["url"] = url.String
		}
		if lastTriggered.Valid {
			wh["last_triggered"] = lastTriggered.Time
		}
		webhooks = append(webhooks, wh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing of_webhooks: %w", err)
	}

	return tools.Ok(webhooks), nil
}

func webhookDelete(ctx *tools.ToolContext, name string) (*tools.Result, error) {
	res, err := ctx.DB.Exec(
		"DELETE FROM of_webhooks WHERE name = ?",
		name,
	)
	if err != nil {
		return nil, fmt.Errorf("deleting webhook: %w", err)
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.NotFound("webhook", name), nil
	}

	return tools.Ok(map[string]any{"deleted": name}), nil
}

// webhookUpdate takes typed args straight from WebhookUpdate. An empty url and
// a nil isEnabled mean "leave unchanged", matching the prior map path where the
// Run method only forwarded those keys when set.
func webhookUpdate(ctx *tools.ToolContext, name, url string, isEnabled *bool) (*tools.Result, error) {
	var setClauses []string
	var values []any

	if url != "" {
		setClauses = append(setClauses, "url = ?")
		values = append(values, url)
	}
	if isEnabled != nil {
		setClauses = append(setClauses, "is_enabled = ?")
		values = append(values, *isEnabled)
	}

	if len(setClauses) == 0 {
		return tools.Fail("provide at least one field to update (url or is_enabled)"), nil
	}

	values = append(values, name)
	query := fmt.Sprintf("UPDATE of_webhooks SET %s WHERE name = ?",
		strings.Join(setClauses, ", "))

	res, err := ctx.DB.Exec(query, values...)
	if err != nil {
		return nil, fmt.Errorf("updating webhook: %w", err)
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.NotFound("webhook", name), nil
	}

	return tools.Ok(map[string]any{"updated": name}), nil
}

func webhookSubscribe(ctx *tools.ToolContext, name string, eventTypesRaw []any) (*tools.Result, error) {
	if len(eventTypesRaw) == 0 {
		return tools.Fail("event_types is required"), nil
	}

	var webhookID int
	err := ctx.DB.QueryRow(
		"SELECT id FROM of_webhooks WHERE name = ?",
		name,
	).Scan(&webhookID)
	if err == sql.ErrNoRows {
		return tools.NotFound("webhook", name), nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying webhook: %w", err)
	}

	var subscribed []string
	for _, et := range eventTypesRaw {
		eventType, ok := et.(string)
		if !ok || eventType == "" {
			continue
		}
		_, err := ctx.DB.Exec(
			"INSERT INTO of_webhook_subscriptions (webhook_id, event_type) VALUES (?, ?) ON CONFLICT(webhook_id, event_type) DO NOTHING",
			webhookID, eventType,
		)
		if err == nil {
			subscribed = append(subscribed, eventType)
		}
	}

	return tools.Ok(map[string]any{
		"name":       name,
		"subscribed": subscribed,
	}), nil
}

func webhookTest(ctx *tools.ToolContext, name, requestBody string) (*tools.Result, error) {
	var url sql.NullString
	var sealed, direction string
	var isEnabled bool
	err := ctx.DB.QueryRow(
		"SELECT url, secret_encrypted, direction, is_enabled FROM of_webhooks WHERE name = ?",
		name,
	).Scan(&url, &sealed, &direction, &isEnabled)
	if err == sql.ErrNoRows {
		return tools.NotFound("webhook", name), nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying webhook: %w", err)
	}
	secret, err := ctx.Encryptor.OpenOptional(sealed)
	if err != nil {
		return tools.Fail("the webhook's signing secret cannot be decrypted (was the encryption key replaced?): %v", err), nil
	}

	if direction != "outgoing" {
		return tools.Fail("test only works for outgoing of_webhooks"), nil
	}
	if !url.Valid || url.String == "" {
		return tools.Fail("webhook has no URL configured"), nil
	}

	// Use the caller-supplied request_body when provided, otherwise a synthetic
	// default test payload.
	var body []byte
	if strings.TrimSpace(requestBody) != "" {
		body = []byte(requestBody)
	} else {
		payload := map[string]any{
			"event":      "webhook.test",
			"project_id": ctx.ProjectID,
			"timestamp":  time.Now().UTC().Format(time.RFC3339),
			"payload": map[string]any{
				"test":    true,
				"message": "Test delivery from Open Fabrica",
			},
		}
		body, _ = json.Marshal(payload)
	}

	// SSRF guard: the webhook URL is stored config and must not be used to probe
	// internal services from the server when an admin clicks "test".
	if err := netguard.ValidateExternalURL(url.String); err != nil {
		return tools.Fail("blocked URL: %v", err), nil
	}

	req, err := http.NewRequest("POST", url.String, bytes.NewReader(body))
	if err != nil {
		return tools.Fail("creating request: %v", err), nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenFabrica-Webhook/1.0")

	// Sign with HMAC if secret is available.
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-Webhook-Signature", "sha256="+sig)
	}

	client := netguard.SSRFSafeClient()
	client.Timeout = 10 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return tools.Fail("delivery failed: %v", err), nil
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	// Log the test delivery (best-effort; the delivery result is still reported).
	_, _ = ctx.DB.Exec(
		"INSERT INTO of_webhook_logs (webhook_id, direction, event_type, payload, status_code, response, success) SELECT id, 'outgoing', 'webhook.test', ?, ?, ?, ? FROM of_webhooks WHERE name = ?",
		string(body), resp.StatusCode, string(respBody), resp.StatusCode >= 200 && resp.StatusCode < 300, name,
	)

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	return &tools.Result{Success: success, Data: map[string]any{
		"name":        name,
		"url":         url.String,
		"status_code": resp.StatusCode,
		"response":    string(respBody),
	}}, nil
}
