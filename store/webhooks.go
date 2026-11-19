package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"openfabrica/db"
)

// ErrWebhookNotFound is returned when a webhook row does not exist.
var ErrWebhookNotFound = errors.New("webhook not found")

// WebhookTarget holds the fields needed to dispatch a test webhook.
type WebhookTarget struct {
	Name            string
	URL             string
	Direction       string
	SecretEncrypted string
}

// WebhookUpdate describes partial updates to a webhook and its subscriptions.
type WebhookUpdate struct {
	Name            *string
	URL             *string
	SecretEncrypted *string
	Events          *[]string
}

// ListWebhooks returns all webhooks for a project ordered newest-first.
func ListWebhooks(pdb *db.ProjectDB) []map[string]any {
	rows, err := pdb.Query(
		`SELECT id, name, secret_encrypted <> '', url, direction, is_enabled, last_triggered, created_at
		 FROM of_webhooks ORDER BY created_at DESC`,
	)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()

	webhooks := []map[string]any{}
	for rows.Next() {
		var id int
		var name string
		var hasSecret bool
		var url, direction sql.NullString
		var isEnabled bool
		var lastTriggered sql.NullTime
		var createdAt time.Time
		if rows.Scan(&id, &name, &hasSecret, &url, &direction, &isEnabled, &lastTriggered, &createdAt) != nil {
			continue
		}
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
	if rows.Err() != nil {
		return []map[string]any{}
	}
	return webhooks
}

// GetWebhook returns a webhook with its subscriptions and recent logs.
func GetWebhook(pdb *db.ProjectDB, webhookID int) (map[string]any, error) {
	var id int
	var name string
	var hasSecret bool
	var url, direction sql.NullString
	var isEnabled bool
	var lastTriggered sql.NullTime
	var createdAt time.Time
	err := pdb.QueryRow(
		`SELECT id, name, secret_encrypted <> '', url, direction, is_enabled, last_triggered, created_at
		 FROM of_webhooks WHERE id = ?`, webhookID,
	).Scan(&id, &name, &hasSecret, &url, &direction, &isEnabled, &lastTriggered, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWebhookNotFound
	}
	if err != nil {
		return nil, err
	}

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

	if subRows, err := pdb.Query("SELECT event_type FROM of_webhook_subscriptions WHERE webhook_id = ?", webhookID); err == nil {
		var subs []string
		for subRows.Next() {
			var ev string
			if subRows.Scan(&ev) == nil {
				subs = append(subs, ev)
			}
		}
		_ = subRows.Err()
		subRows.Close()
		wh["subscriptions"] = subs
	}

	if logRows, err := pdb.Query(
		`SELECT id, direction, event_type, status_code, success, created_at
		 FROM of_webhook_logs WHERE webhook_id = ? ORDER BY created_at DESC LIMIT 25`, webhookID,
	); err == nil {
		var logs []map[string]any
		for logRows.Next() {
			var logID int
			var logDir, eventType string
			var statusCode sql.NullInt64
			var success bool
			var logCreatedAt time.Time
			if logRows.Scan(&logID, &logDir, &eventType, &statusCode, &success, &logCreatedAt) == nil {
				entry := map[string]any{
					"id": logID, "direction": logDir, "event_type": eventType,
					"success": success, "created_at": logCreatedAt,
				}
				if statusCode.Valid {
					entry["status_code"] = statusCode.Int64
				}
				logs = append(logs, entry)
			}
		}
		_ = logRows.Err()
		logRows.Close()
		wh["logs"] = logs
	}

	return wh, nil
}

// ToggleWebhook flips is_enabled atomically and returns the new state.
func ToggleWebhook(pdb *db.ProjectDB, webhookID int) (bool, error) {
	if _, err := pdb.ExecWrite("UPDATE of_webhooks SET is_enabled = NOT is_enabled WHERE id = ?", webhookID); err != nil {
		return false, err
	}
	var isEnabled bool
	if err := pdb.QueryRow("SELECT is_enabled FROM of_webhooks WHERE id = ?", webhookID).Scan(&isEnabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrWebhookNotFound
		}
		return false, err
	}
	return isEnabled, nil
}

// DeleteWebhook deletes a webhook by ID.
func DeleteWebhook(pdb *db.ProjectDB, webhookID int) error {
	_, err := pdb.ExecWrite("DELETE FROM of_webhooks WHERE id = ?", webhookID)
	return err
}

// LoadWebhookTarget returns the metadata needed to send a test webhook.
func LoadWebhookTarget(pdb *db.ProjectDB, webhookID int) (*WebhookTarget, error) {
	var t WebhookTarget
	var url, direction, sealed sql.NullString
	err := pdb.QueryRow(
		"SELECT name, url, direction, secret_encrypted FROM of_webhooks WHERE id = ?", webhookID,
	).Scan(&t.Name, &url, &direction, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWebhookNotFound
	}
	if err != nil {
		return nil, err
	}
	t.URL = url.String
	t.Direction = direction.String
	t.SecretEncrypted = sealed.String
	return &t, nil
}

// RecordWebhookTestLog inserts a test delivery log row (best-effort).
func RecordWebhookTestLog(pdb *db.ProjectDB, webhookID int, payload string, statusCode int, response string, success bool) {
	_, _ = pdb.ExecWrite(
		"INSERT INTO of_webhook_logs (webhook_id, direction, event_type, payload, status_code, response, success) VALUES (?, 'outgoing', 'test', ?, ?, ?, ?)",
		webhookID, payload, statusCode, response, success,
	)
}

// UpdateWebhook updates a webhook and (if non-nil) replaces its event subscriptions atomically.
func UpdateWebhook(pdb *db.ProjectDB, webhookID int, u WebhookUpdate) error {
	return pdb.WithTx(func(tx *sql.Tx) error {
		var sets []string
		var args []any
		if u.Name != nil {
			sets = append(sets, "name = ?")
			args = append(args, *u.Name)
		}
		if u.URL != nil {
			sets = append(sets, "url = ?")
			args = append(args, *u.URL)
		}
		if u.SecretEncrypted != nil {
			sets = append(sets, "secret_encrypted = ?")
			args = append(args, *u.SecretEncrypted)
		}
		if len(sets) > 0 {
			args = append(args, webhookID)
			query := "UPDATE of_webhooks SET " + strings.Join(sets, ", ") + " WHERE id = ?"
			if _, err := tx.Exec(query, args...); err != nil {
				return err
			}
		}
		if u.Events != nil {
			if _, err := tx.Exec("DELETE FROM of_webhook_subscriptions WHERE webhook_id = ?", webhookID); err != nil {
				return err
			}
			for _, evt := range *u.Events {
				if _, err := tx.Exec("INSERT INTO of_webhook_subscriptions (webhook_id, event_type) VALUES (?, ?)", webhookID, evt); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// CreateWebhook inserts a webhook and its event subscriptions atomically.
func CreateWebhook(pdb *db.ProjectDB, name, direction, url, secretEncrypted string, events []string) (int64, error) {
	var whID int64
	err := pdb.WithTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"INSERT INTO of_webhooks (name, direction, url, secret_encrypted) VALUES (?, ?, ?, ?)",
			name, direction, url, secretEncrypted,
		)
		if err != nil {
			return err
		}
		whID, _ = res.LastInsertId()
		for _, evt := range events {
			if _, err := tx.Exec("INSERT INTO of_webhook_subscriptions (webhook_id, event_type) VALUES (?, ?)", whID, evt); err != nil {
				return err
			}
		}
		return nil
	})
	return whID, err
}

// IncomingWebhook holds the verification metadata for an incoming webhook.
type IncomingWebhook struct {
	ID              int
	SecretEncrypted string
	IsEnabled       bool
}

// LoadIncomingWebhook looks up an incoming webhook by name.
func LoadIncomingWebhook(pdb *db.ProjectDB, name string) (*IncomingWebhook, error) {
	var wh IncomingWebhook
	err := pdb.QueryRow(
		"SELECT id, secret_encrypted, is_enabled FROM of_webhooks WHERE name = ? AND COALESCE(direction, 'incoming') = 'incoming'",
		name,
	).Scan(&wh.ID, &wh.SecretEncrypted, &wh.IsEnabled)
	if err != nil {
		return nil, err
	}
	return &wh, nil
}

// TouchIncomingWebhookTriggered updates last_triggered on an incoming webhook.
func TouchIncomingWebhookTriggered(pdb *db.ProjectDB, webhookID int) error {
	_, err := pdb.ExecWrite("UPDATE of_webhooks SET last_triggered = CURRENT_TIMESTAMP WHERE id = ?", webhookID)
	return err
}

// RecordIncomingWebhookLog records a verified incoming webhook delivery.
func RecordIncomingWebhookLog(pdb *db.ProjectDB, webhookID int, body string) error {
	_, err := pdb.ExecWrite(
		"INSERT INTO of_webhook_logs (webhook_id, direction, event_type, payload, success) VALUES (?, 'incoming', 'webhook.received', ?, 1)",
		webhookID, body,
	)
	return err
}
