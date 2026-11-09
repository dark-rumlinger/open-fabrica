package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"openfabrica/internal/dbh"
)

// Provider webhook protocol: signature schemes, idempotency keys, and the
// per-provider payload shapes a reconcile reads.

// stripeReplayToleranceSeconds is the freshness window for the t= timestamp in
// a Stripe-Signature header. Stripe's docs recommend 5 minutes; anything older
// is rejected to block replay of captured webhooks.
const stripeReplayToleranceSeconds = 300

// verifyWebhookSignature returns true when signature matches HMAC-SHA256(body, secret)
// for the configured providerType. Stripe uses a t=<ts>,v1=<sig> envelope with
// a replay-protection window; other providers sign the raw body.
func verifyWebhookSignature(providerType, body, signature, secret string) bool {
	if providerType == "stripe" {
		var ts string
		var sigs []string
		for _, part := range strings.Split(signature, ",") {
			switch {
			case strings.HasPrefix(part, "t="):
				ts = part[2:]
			case strings.HasPrefix(part, "v1="):
				sigs = append(sigs, part[3:])
			}
		}
		if ts == "" || len(sigs) == 0 {
			return false
		}
		tsInt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return false
		}
		if abs64(time.Now().Unix()-tsInt) > stripeReplayToleranceSeconds {
			return false
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "." + body))
		expected := hex.EncodeToString(mac.Sum(nil))
		for _, s := range sigs {
			if hmac.Equal([]byte(s), []byte(expected)) {
				return true
			}
		}
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// webhookEventKey derives a stable, provider-scoped idempotency key for a webhook
// delivery. Stripe and PayPal both carry a top-level event "id" (evt_… / WH-…);
// when absent, the raw body is hashed so a byte-identical replay maps to the same
// key. The provider prefix keeps keys from colliding across providers.
func webhookEventKey(providerType, body string, data map[string]any) string {
	if id, _ := data["id"].(string); id != "" {
		return providerType + ":" + id
	}
	sum := sha256.Sum256([]byte(body))
	return providerType + ":" + hex.EncodeToString(sum[:])
}

// claimWebhookEvent records key as processed and reports whether this call won
// the claim (true) or the event was already handled (false). Atomic via INSERT
// OR IGNORE on the PRIMARY KEY, so concurrent duplicate deliveries can't both
// win. Call after signature verification so unverified bodies never fill the
// table.
func claimWebhookEvent(db dbh.Handle, key string) (bool, error) {
	res, err := db.Exec("INSERT OR IGNORE INTO of_processed_webhooks (event_key) VALUES (?)", key)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// releaseWebhookEvent removes a claim so a later retry can reprocess. Used only
// when processing failed after the claim, so the provider's redelivery is not
// silently swallowed as a duplicate.
func releaseWebhookEvent(db dbh.Handle, key string) {
	_, _ = db.Exec("DELETE FROM of_processed_webhooks WHERE event_key = ?", key)
}

// parseSubscriptionWebhookEvent returns the subscription external ID and the
// normalized status implied by a webhook payload, or "","" when the event is
// not a subscription lifecycle event.
func parseSubscriptionWebhookEvent(providerType string, data map[string]any) (externalID, status string) {
	switch providerType {
	case "stripe":
		eventType := str(data, "type")
		obj, _ := data["data"].(map[string]any)
		if obj == nil {
			return
		}
		inner, _ := obj["object"].(map[string]any)
		if inner == nil {
			return
		}
		switch eventType {
		case "invoice.paid":
			externalID = invoiceSubscriptionID(inner)
			status = "active"
		case "invoice.payment_failed":
			externalID = invoiceSubscriptionID(inner)
			status = "past_due"
		case "customer.subscription.deleted":
			externalID = str(inner, "id")
			status = "canceled"
		case "customer.subscription.updated":
			externalID = str(inner, "id")
			status = str(inner, "status")
		}
	case "paypal":
		eventType := str(data, "event_type")
		resource, _ := data["resource"].(map[string]any)
		if resource == nil {
			return
		}
		switch eventType {
		case "BILLING.SUBSCRIPTION.ACTIVATED":
			externalID = str(resource, "id")
			status = "active"
		case "BILLING.SUBSCRIPTION.CANCELLED", "BILLING.SUBSCRIPTION.EXPIRED":
			externalID = str(resource, "id")
			status = "canceled"
		}
	}
	return
}

// invoiceSubscriptionID reads the subscription an invoice belongs to, from the
// top-level field or the nested parent block newer API versions use.
func invoiceSubscriptionID(inv map[string]any) string {
	if id := str(inv, "subscription"); id != "" {
		return id
	}
	parent, _ := inv["parent"].(map[string]any)
	details, _ := parent["subscription_details"].(map[string]any)
	return str(details, "subscription")
}

// stripeEventObject returns the event type and its data.object.
func stripeEventObject(data map[string]any) (string, map[string]any) {
	obj, _ := data["data"].(map[string]any)
	inner, _ := obj["object"].(map[string]any)
	return str(data, "type"), inner
}

// subscriptionLink is a completed subscription checkout: it ties the checkout
// session id stored at creation to the provider's subscription id.
type subscriptionLink struct {
	sessionID      string
	subscriptionID string
	status         string
}

// parseSubscriptionLink recognizes a completed Stripe subscription checkout.
func parseSubscriptionLink(providerType string, data map[string]any) (subscriptionLink, bool) {
	if providerType != "stripe" {
		return subscriptionLink{}, false
	}
	typ, inner := stripeEventObject(data)
	if typ != "checkout.session.completed" || inner == nil || str(inner, "mode") != "subscription" {
		return subscriptionLink{}, false
	}
	link := subscriptionLink{sessionID: str(inner, "id"), subscriptionID: str(inner, "subscription"), status: "pending"}
	if link.sessionID == "" || link.subscriptionID == "" {
		return subscriptionLink{}, false
	}
	// A trial reports no_payment_required; a delayed method stays unpaid until
	// its own invoice.paid arrives.
	if ps := str(inner, "payment_status"); ps == "paid" || ps == "no_payment_required" {
		link.status = "active"
	}
	return link, true
}

// isRenewalInvoice reports an invoice.paid for a cycle after the first one.
func isRenewalInvoice(providerType string, data map[string]any) bool {
	if providerType != "stripe" {
		return false
	}
	typ, inner := stripeEventObject(data)
	if typ != "invoice.paid" || inner == nil {
		return false
	}
	reason := str(inner, "billing_reason")
	return reason != "" && reason != "subscription_create"
}

// parseRenewalPeriodEnd digs out the next-period-end timestamp from a
// renewal webhook so the local of_subscriptions row stays in sync.
func parseRenewalPeriodEnd(providerType string, data map[string]any) time.Time {
	if providerType != "stripe" {
		return time.Time{}
	}
	obj, _ := data["data"].(map[string]any)
	if obj == nil {
		return time.Time{}
	}
	inner, _ := obj["object"].(map[string]any)
	if inner == nil {
		return time.Time{}
	}
	lines, _ := inner["lines"].(map[string]any)
	if lines == nil {
		return time.Time{}
	}
	lineData, _ := lines["data"].([]any)
	if len(lineData) == 0 {
		return time.Time{}
	}
	line, _ := lineData[0].(map[string]any)
	if line == nil {
		return time.Time{}
	}
	pe, _ := line["period"].(map[string]any)
	if pe == nil {
		return time.Time{}
	}
	if end, ok := pe["end"].(float64); ok {
		return time.Unix(int64(end), 0)
	}
	return time.Time{}
}

// parseWebhookStatus extracts the external payment id and the normalized
// status from a one-shot payment webhook (not subscriptions).
func parseWebhookStatus(providerType string, data map[string]any) (externalID, status string) {
	switch providerType {
	case "stripe":
		typ, inner := stripeEventObject(data)
		if inner == nil {
			return
		}
		externalID = str(inner, "id")
		switch typ {
		case "checkout.session.completed":
			// Delayed methods complete the session unpaid; the settled result
			// arrives as an async_payment event, so unpaid stays unresolved.
			if ps := str(inner, "payment_status"); ps == "paid" || ps == "no_payment_required" {
				status = "paid"
			}
		case "checkout.session.async_payment_succeeded":
			status = "paid"
		case "checkout.session.async_payment_failed":
			status = "failed"
		case "checkout.session.expired":
			status = "expired"
		}
	case "paypal":
		if resource, ok := data["resource"].(map[string]any); ok {
			externalID = str(resource, "id")
			if s := str(resource, "status"); s != "" {
				if s == "COMPLETED" {
					status = "paid"
				} else {
					status = strings.ToLower(s)
				}
			}
		}
	case "mollie":
		externalID = str(data, "id")
		status = str(data, "status")
	default:
		externalID = str(data, "id")
		if externalID == "" {
			externalID = str(data, "payment_id")
		}
		status = str(data, "status")
	}
	return
}

// str returns data[key] when it is a non-empty string, else "".
func str(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}
