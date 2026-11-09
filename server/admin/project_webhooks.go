package admin

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"time"

	"openfabrica/webhooks"
)

type ProjectWebhooksHandler struct {
	deps *Deps
}

func (h *ProjectWebhooksHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	rows, err := projectDB.Query(
		`SELECT id, name, secret_encrypted <> '', url, direction, is_enabled, last_triggered, created_at
		 FROM of_webhooks ORDER BY created_at DESC`)
	if err != nil {
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	defer rows.Close()
	var webhooks []map[string]any
	for rows.Next() {
		var id int
		var name string
		var hasSecret bool
		var url, direction sql.NullString
		var isEnabled bool
		var lastTriggered sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &hasSecret, &url, &direction, &isEnabled, &lastTriggered, &createdAt); err != nil {
			continue
		}
		// Never echo the raw HMAC signing secret back in list responses (these
		// routes are readable by non-admin users). Expose only whether one is set;
		// the full secret is returned exactly once, at creation time.
		wh := map[string]any{
			"id": id, "name": name, "has_secret": hasSecret,
			"is_enabled": isEnabled, "created_at": createdAt,
		}
		if url.Valid {
			wh["url"] = url.String
		}
		if direction.Valid {
			wh["direction"] = direction.String
		} else {
			wh["direction"] = "incoming"
		}
		if lastTriggered.Valid {
			wh["last_triggered"] = lastTriggered.Time
		}
		webhooks = append(webhooks, wh)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusOK, []map[string]any{})
		return
	}
	if webhooks == nil {
		webhooks = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, webhooks)
}

func (h *ProjectWebhooksHandler) Get(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}
	var id int
	var name string
	var hasSecret bool
	var url, direction sql.NullString
	var isEnabled bool
	var lastTriggered sql.NullTime
	var createdAt time.Time
	err := projectDB.QueryRow(
		`SELECT id, name, secret_encrypted <> '', url, direction, is_enabled, last_triggered, created_at
		 FROM of_webhooks WHERE id = ?`, webhookID,
	).Scan(&id, &name, &hasSecret, &url, &direction, &isEnabled, &lastTriggered, &createdAt)
	if err != nil {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	// As in List, do not return the raw signing secret; only whether one is set.
	wh := map[string]any{
		"id": id, "name": name, "has_secret": hasSecret,
		"is_enabled": isEnabled, "created_at": createdAt,
	}
	if url.Valid {
		wh["url"] = url.String
	}
	if direction.Valid {
		wh["direction"] = direction.String
	} else {
		wh["direction"] = "incoming"
	}
	if lastTriggered.Valid {
		wh["last_triggered"] = lastTriggered.Time
	}

	// Load subscriptions.
	subRows, err := projectDB.Query(
		"SELECT event_type FROM of_webhook_subscriptions WHERE webhook_id = ?", webhookID)
	if err == nil {
		wh["subscriptions"] = collectRows(subRows, func(rows *sql.Rows) (string, error) {
			var eventType string
			if err := rows.Scan(&eventType); err != nil {
				return eventType, err
			}
			return eventType, nil
		})
	}

	// Load recent logs.
	logRows, err := projectDB.Query(
		`SELECT id, direction, event_type, status_code, success, created_at
		 FROM of_webhook_logs WHERE webhook_id = ? ORDER BY created_at DESC LIMIT 25`, webhookID)
	if err == nil {
		var logs []map[string]any
		for logRows.Next() {
			var logID int
			var logDir, eventType string
			var statusCode sql.NullInt64
			var success bool
			var logCreatedAt time.Time
			if err := logRows.Scan(&logID, &logDir, &eventType, &statusCode, &success, &logCreatedAt); err == nil {
				log := map[string]any{
					"id": logID, "direction": logDir, "event_type": eventType,
					"success": success, "created_at": logCreatedAt,
				}
				if statusCode.Valid {
					log["status_code"] = statusCode.Int64
				}
				logs = append(logs, log)
			}
		}
		logRows.Close()
		_ = logRows.Err() // recent logs are advisory; degrade silently like the query error above
		wh["logs"] = logs
	}

	writeJSON(w, http.StatusOK, wh)
}

func (h *ProjectWebhooksHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}
	_, err := projectDB.ExecWrite(
		"UPDATE of_webhooks SET is_enabled = NOT is_enabled WHERE id = ?",
		webhookID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to toggle webhook")
		return
	}
	var isEnabled bool
	if err := projectDB.QueryRow("SELECT is_enabled FROM of_webhooks WHERE id = ?", webhookID).Scan(&isEnabled); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read webhook state")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": webhookID, "is_enabled": isEnabled})
}

func (h *ProjectWebhooksHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}
	_, err := projectDB.ExecWrite(
		"DELETE FROM of_webhooks WHERE id = ?",
		webhookID,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete webhook")
		return
	}
	writeStatus(w, "deleted")
}

// Test sends one signed test event to an outgoing webhook's URL, from the
// server. The browser cannot do this itself: the admin CSP only allows requests
// to its own origin, and an unsigned browser request would not prove the
// receiver verifies the signature anyway. The attempt is recorded in the
// webhook's delivery log.
func (h *ProjectWebhooksHandler) Test(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}

	var name string
	var url, direction, sealed sql.NullString
	err := projectDB.QueryRow(
		"SELECT name, url, direction, secret_encrypted FROM of_webhooks WHERE id = ?", webhookID,
	).Scan(&name, &url, &direction, &sealed)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load webhook")
		return
	}
	if direction.Valid && direction.String == "incoming" {
		writeError(w, http.StatusBadRequest, "only outgoing webhooks can be tested; incoming ones receive requests")
		return
	}
	if !url.Valid || url.String == "" {
		writeError(w, http.StatusBadRequest, "this webhook has no URL to send to")
		return
	}

	secret := ""
	if sealed.Valid {
		opened, openErr := h.deps.Encryptor.OpenOptional(sealed.String)
		if openErr != nil {
			writeError(w, http.StatusInternalServerError, "the signing secret cannot be decrypted")
			return
		}
		secret = opened
	}

	result := webhooks.SendTest(r.Context(), url.String, secret, projectID, name)

	// Best-effort: the log is a convenience, the answer to the admin is not.
	payload := "test event"
	_, _ = projectDB.ExecWrite(
		"INSERT INTO of_webhook_logs (webhook_id, direction, event_type, payload, status_code, response, success) VALUES (?, 'outgoing', 'test', ?, ?, ?, ?)",
		webhookID, payload, result.StatusCode, result.Response, result.Success,
	)

	writeJSON(w, http.StatusOK, result)
}

type updateWebhookRequest struct {
	Name   *string   `json:"name"`
	URL    *string   `json:"url"`
	Secret *string   `json:"secret"`
	Events *[]string `json:"events"`
}

// Update modifies an existing webhook.
func (h *ProjectWebhooksHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}
	webhookID, ok := requireIntParam(w, r, "webhookID", "invalid webhook ID")
	if !ok {
		return
	}

	var req updateWebhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// Build dynamic update query.
	sets := []string{}
	args := []any{}
	if req.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *req.Name)
	}
	if req.URL != nil {
		sets = append(sets, "url = ?")
		args = append(args, *req.URL)
	}
	if req.Secret != nil {
		sealed, err := h.deps.Encryptor.SealOptional(*req.Secret)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to protect webhook secret")
			return
		}
		sets = append(sets, "secret_encrypted = ?")
		args = append(args, sealed)
	}

	if len(sets) > 0 {
		query := "UPDATE of_webhooks SET " + sets[0]
		for _, s := range sets[1:] {
			query += ", " + s
		}
		query += " WHERE id = ?"
		args = append(args, webhookID)
		_, err := projectDB.ExecWrite(query, args...)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update webhook")
			return
		}
	}

	// Update event subscriptions only when the caller sends an events array.
	// A nil pointer means "not provided" (leave subscriptions untouched); an
	// empty array means "clear all subscriptions".
	if req.Events != nil {
		if _, err := projectDB.ExecWrite("DELETE FROM of_webhook_subscriptions WHERE webhook_id = ?", webhookID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update webhook subscriptions")
			return
		}
		for _, evt := range *req.Events {
			if _, err := projectDB.ExecWrite("INSERT INTO of_webhook_subscriptions (webhook_id, event_type) VALUES (?, ?)", webhookID, evt); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to update webhook subscriptions")
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": webhookID, "updated": true})
}

type createWebhookRequest struct {
	Name      string   `json:"name"`
	Direction string   `json:"direction"` // incoming or outgoing
	URL       string   `json:"url"`       // for outgoing
	Secret    string   `json:"secret"`    // HMAC signing secret
	Events    []string `json:"events"`    // event subscriptions
}

// Create adds a new webhook for a project.
func (h *ProjectWebhooksHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var req createWebhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Direction == "" {
		req.Direction = "incoming"
	}
	if req.Direction == "outgoing" && req.URL == "" {
		writeError(w, http.StatusBadRequest, "url is required for outgoing webhooks")
		return
	}

	// Always store a signing secret. Incoming webhooks are rejected without one
	// (the public handler verifies HMAC), and outgoing deliveries sign with it.
	// Generate a strong secret when the caller did not supply their own.
	secret := req.Secret
	if secret == "" {
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to generate webhook secret")
			return
		}
		secret = hex.EncodeToString(secretBytes)
	}

	sealed, err := h.deps.Encryptor.SealOptional(secret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to protect webhook secret")
		return
	}
	db := projectDB.Writer()
	result, err := db.Exec(
		"INSERT INTO of_webhooks (name, direction, url, secret_encrypted) VALUES (?, ?, ?, ?)",
		req.Name, req.Direction, req.URL, sealed,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create webhook")
		return
	}
	whID, _ := result.LastInsertId()

	// Add event subscriptions. A silent failure here would leave the webhook
	// created but subscribed to nothing while the admin sees success.
	for _, evt := range req.Events {
		if _, err := db.Exec("INSERT INTO of_webhook_subscriptions (webhook_id, event_type) VALUES (?, ?)", whID, evt); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create webhook subscription")
			return
		}
	}

	// Return the secret so the operator can configure the sender to sign with it.
	writeJSON(w, http.StatusOK, map[string]any{"id": whID, "name": req.Name, "secret": secret})
}
