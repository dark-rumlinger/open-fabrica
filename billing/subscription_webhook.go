package billing

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"openfabrica/events"
)

// subscriptionChange is one reconciled subscription webhook.
type subscriptionChange struct {
	externalID string
	sessionID  string // set on checkout completion, to link the stored session id
	status     string
	renewal    bool // invoice for a cycle after the first
}

// subscriptionEventFor maps a subscription status onto the payment event a
// fulfillment trigger listens for, or "" for a status that settles nothing.
func subscriptionEventFor(status string) events.EventType {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active", "trialing":
		return events.EventPaymentCompleted
	case "canceled", "past_due", "unpaid", "incomplete_expired":
		return events.EventPaymentFailed
	}
	return ""
}

// applySubscriptionChange writes a subscription change and raises its event.
// It reports false when no local row matches the provider id.
//
// An event is raised on a status transition, or on a renewal invoice, so the
// first payment (checkout completion plus its invoice) raises it once.
func applySubscriptionChange(env WebhookEnv, ch subscriptionChange, periodEnd func() (time.Time, bool)) (bool, error) {
	if ch.sessionID != "" {
		if _, err := env.DB.Exec(
			"UPDATE of_subscriptions SET external_id = ?, updated_at = CURRENT_TIMESTAMP WHERE external_id = ? AND status != 'canceled'",
			ch.externalID, ch.sessionID,
		); err != nil {
			return false, err
		}
	}

	var current string
	err := env.DB.QueryRow("SELECT status FROM of_subscriptions WHERE external_id = ?", ch.externalID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Canceled is terminal: a replayed or out-of-order event must not revive it.
	if current == "canceled" {
		return true, nil
	}

	changed := current != ch.status
	if changed {
		if _, err := env.DB.Exec(
			"UPDATE of_subscriptions SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE external_id = ?",
			ch.status, ch.externalID,
		); err != nil {
			return false, err
		}
	}
	if ch.status == "canceled" {
		if _, err := env.DB.Exec(
			"UPDATE of_subscriptions SET canceled_at = CURRENT_TIMESTAMP WHERE external_id = ? AND canceled_at IS NULL",
			ch.externalID,
		); err != nil {
			return false, err
		}
	}
	if ch.renewal {
		if end, ok := periodEnd(); ok {
			if _, err := env.DB.Exec(
				"UPDATE of_subscriptions SET current_period_start = CURRENT_TIMESTAMP, current_period_end = ? WHERE external_id = ?",
				end, ch.externalID,
			); err != nil {
				return false, err
			}
		}
	}

	if changed || ch.renewal {
		publishSubscriptionEvent(env, ch.externalID, ch.status)
	}
	return true, nil
}

// publishSubscriptionEvent raises payment.completed / payment.failed for a
// subscription. The payload spreads the subscription's metadata first, then
// stamps the platform fields over it.
func publishSubscriptionEvent(env WebhookEnv, externalID, status string) {
	evType := subscriptionEventFor(status)
	if evType == "" || env.Bus == nil {
		return
	}
	payload := map[string]any{}

	var id int64
	var planID, userID sql.NullInt64
	var metadata sql.NullString
	if err := env.DB.QueryRow(
		"SELECT id, plan_id, user_id, metadata FROM of_subscriptions WHERE external_id = ?",
		externalID,
	).Scan(&id, &planID, &userID, &metadata); err == nil {
		if metadata.Valid && metadata.String != "" {
			var meta map[string]any
			if json.Unmarshal([]byte(metadata.String), &meta) == nil {
				for k, v := range meta {
					payload[k] = v
				}
			}
		}
		payload["subscription_id"] = id
		if planID.Valid {
			payload["plan_id"] = planID.Int64
		}
		if userID.Valid {
			payload["user_id"] = userID.Int64
		}
	}
	payload["external_id"] = externalID
	payload["status"] = status
	if evType == events.EventPaymentFailed {
		payload["reason"] = status
	}
	env.Bus.Publish(events.NewEvent(evType, env.ProjectID, payload))
}
