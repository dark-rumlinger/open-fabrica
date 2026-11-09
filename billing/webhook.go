package billing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"openfabrica/events"
	"openfabrica/internal/dbh"
	"openfabrica/security"
)

// WebhookPath is the fixed, platform-owned route a provider posts its signed
// callbacks to. The public server mounts it and manage_payments hands it to the
// model on configure, so the path exists in one place rather than in a route table
// and a piece of prose that can disagree.
const WebhookPath = "/payments/webhook"

// Kinds a reconciled webhook can resolve to.
const (
	WebhookPayment      = "payment"
	WebhookSubscription = "subscription"
	WebhookDuplicate    = "duplicate"
	WebhookIgnored      = "ignored"
)

// WebhookEnv is the runtime a webhook reconcile needs. Named separately from any
// caller's own context because reconciling is not tied to how the webhook arrived:
// the same work happens whether a provider POSTed it or an operator pasted it.
type WebhookEnv struct {
	DB        dbh.Handle
	Encryptor *security.Encryptor
	Bus       *events.Bus
	Logger    *slog.Logger
	ProjectID int
}

// WebhookOutcome reports what reconciling one webhook did.
type WebhookOutcome struct {
	Kind       string // one of the Webhook* constants
	ExternalID string
	Status     string
	EventKey   string // set when Kind == WebhookDuplicate
}

// WebhookRefusal is a webhook the platform declines to act on: payments not
// configured, an unsupported provider family, a missing or unverifiable signature.
// Distinct from an error because the sender is wrong rather than the platform, and
// a transport that answers with a status code needs to tell those apart.
type WebhookRefusal struct{ Msg string }

func (e *WebhookRefusal) Error() string { return e.Msg }

func refusef(format string, args ...any) error {
	return &WebhookRefusal{Msg: fmt.Sprintf(format, args...)}
}

// ReconcileWebhook verifies a provider webhook's signature and reconciles the
// payment or subscription row it implies, raising payment.completed /
// payment.failed on a settled payment.
//
// This is the single home for that sequence, and the signature check is why: it is
// the only thing separating a real settlement from an attacker who found the
// webhook path, so a transport that re-implemented any part of it would eventually
// re-implement that part too. Every caller reaches money state through here.
func ReconcileWebhook(env WebhookEnv, body, signature string) (*WebhookOutcome, error) {
	log := env.Logger
	if log == nil {
		log = slog.Default()
	}

	var providerType, webhookSecretName string
	if err := env.DB.QueryRow(
		"SELECT provider_type, webhook_secret_name FROM of_payment_config WHERE id = 1",
	).Scan(&providerType, &webhookSecretName); err != nil {
		return nil, refusef("payments not configured")
	}

	// PayPal and Mollie do not sign webhooks with an HMAC of the body: PayPal uses
	// an RSA transmission signature verified via its verify-webhook-signature API,
	// and Mollie sends only a payment id and expects a server-side status fetch.
	// The generic HMAC verification below cannot authenticate either, so instead
	// of silently failing every genuine event - which leaves a paid order stuck
	// "pending" forever - reject with an honest message. Reconciling these
	// providers needs their provider-specific verification (a separate feature);
	// Stripe is fully supported today.
	if providerType == "paypal" || providerType == "mollie" {
		return nil, refusef("automatic webhook reconciliation for provider %q is not supported yet: it uses a verification scheme this build does not implement (unlike Stripe), so its events cannot be authenticated here. Use the Stripe provider for automatic reconciliation, or reconcile these payments manually", providerType)
	}

	// A webhook reconciles money state (marks orders paid, activates or cancels
	// subscriptions), so it is only trusted when its provider signature verifies.
	// Refuse outright when no signing secret is configured rather than mutating
	// rows from an unauthenticated body - an attacker who can POST to the webhook
	// path could otherwise forge "paid" events.
	if webhookSecretName == "" {
		return nil, refusef("refusing to process payment webhook: no webhook_secret_name configured. Run manage_payments(action='configure') with a webhook_secret_name first")
	}
	if signature == "" {
		return nil, refusef("webhook_signature is required")
	}
	if env.Encryptor == nil {
		return nil, refusef("encryptor not available for webhook signature verification")
	}
	var encryptedValue string
	if err := env.DB.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = ?", webhookSecretName).Scan(&encryptedValue); err != nil {
		return nil, refusef("webhook secret %q not found", webhookSecretName)
	}
	secretValue, decErr := env.Encryptor.Decrypt(encryptedValue)
	if decErr != nil {
		return nil, refusef("failed to decrypt webhook secret")
	}
	if !verifyWebhookSignature(providerType, body, signature, secretValue) {
		return nil, refusef("webhook signature verification failed")
	}

	var webhookData map[string]any
	if err := json.Unmarshal([]byte(body), &webhookData); err != nil {
		return nil, refusef("invalid webhook JSON")
	}

	// Decide what the event means before claiming it, so an event type this
	// build does not act on is acknowledged without consuming an idempotency slot.
	link, isLink := parseSubscriptionLink(providerType, webhookData)
	subExtID, subStatus := parseSubscriptionWebhookEvent(providerType, webhookData)
	isSub := isLink || (subExtID != "" && subStatus != "")
	var externalID, newStatus string
	if !isSub {
		externalID, newStatus = parseWebhookStatus(providerType, webhookData)
		if providerType == "stripe" && newStatus == "" {
			return &WebhookOutcome{Kind: WebhookIgnored}, nil
		}
		if externalID == "" || newStatus == "" {
			return nil, refusef("could not extract payment ID or status from webhook")
		}
	}

	// Idempotency: claim this event before mutating any money row so a replayed or
	// duplicated delivery can't double-apply. Runs after signature verification so
	// only authentic events are recorded.
	eventKey := webhookEventKey(providerType, body, webhookData)
	claimed, claimErr := claimWebhookEvent(env.DB, eventKey)
	if claimErr != nil {
		return nil, fmt.Errorf("webhook idempotency check: %w", claimErr)
	}
	if !claimed {
		return &WebhookOutcome{Kind: WebhookDuplicate, EventKey: eventKey}, nil
	}

	if isSub {
		ch := subscriptionChange{externalID: subExtID, status: subStatus, renewal: isRenewalInvoice(providerType, webhookData)}
		if isLink {
			ch = subscriptionChange{externalID: link.subscriptionID, sessionID: link.sessionID, status: link.status}
		}
		matched, err := applySubscriptionChange(env, ch, func() (time.Time, bool) {
			end := parseRenewalPeriodEnd(providerType, webhookData)
			return end, !end.IsZero()
		})
		if err != nil {
			// Release the claim so the provider's redelivery reprocesses.
			releaseWebhookEvent(env.DB, eventKey)
			return nil, fmt.Errorf("updating subscription: %w", err)
		}
		if !matched {
			releaseWebhookEvent(env.DB, eventKey)
			log.Warn("webhook for unknown subscription ignored", "external_id", ch.externalID)
			return &WebhookOutcome{Kind: WebhookIgnored, ExternalID: ch.externalID}, nil
		}
		return &WebhookOutcome{Kind: WebhookSubscription, ExternalID: ch.externalID, Status: ch.status}, nil
	}

	// paid is terminal: a late or out-of-order event never moves a settled
	// payment back.
	res, err := env.DB.Exec(
		"UPDATE of_payments SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE external_id = ? AND status NOT IN ('paid', 'refunded')",
		newStatus, externalID,
	)
	if err != nil {
		// Release the idempotency claim so the provider's redelivery reprocesses
		// rather than being swallowed as a duplicate.
		releaseWebhookEvent(env.DB, eventKey)
		return nil, fmt.Errorf("updating payment status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var known int
		_ = env.DB.QueryRow("SELECT COUNT(*) FROM of_payments WHERE external_id = ?", externalID).Scan(&known)
		if known == 0 {
			releaseWebhookEvent(env.DB, eventKey)
			log.Warn("webhook for unknown payment ignored", "external_id", externalID)
		}
		return &WebhookOutcome{Kind: WebhookIgnored, ExternalID: externalID}, nil
	}
	// Raised only after the row commits and only inside the idempotency claim
	// above, so a redelivered webhook cannot fire fulfillment twice.
	publishPaymentEvent(env, externalID, newStatus)
	return &WebhookOutcome{Kind: WebhookPayment, ExternalID: externalID, Status: newStatus}, nil
}

// paymentEventFor maps a reconciled payment status onto the event a project's
// fulfillment trigger listens for, or "" for a status that has not resolved yet.
//
// "paid" is the settled marker across every provider: Stripe reports it as the
// checkout session's payment_status, parseWebhookStatus normalizes PayPal's
// COMPLETED to it, and Mollie emits it directly. Anything else terminal is a
// failure. A still-moving status (pending/open/processing/created) raises nothing:
// a trigger fires once per event, so announcing an outcome that has not happened is
// worse than announcing it late, and the settled webhook is still coming.
func paymentEventFor(status string) events.EventType {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "paid":
		return events.EventPaymentCompleted
	case "failed", "canceled", "cancelled", "expired", "denied", "declined", "chargeback", "refunded":
		return events.EventPaymentFailed
	}
	return ""
}

// publishPaymentEvent raises payment.completed / payment.failed for a reconciled
// payment. This is the ONLY thing that raises them, and it is the whole reason the
// signed-webhook path exists: the provider's signature is the only trustworthy
// proof a payment happened, so granting the thing paid for hangs off this event
// rather than off the success_url redirect, which any buyer can open without paying.
//
// The payload spreads the checkout's metadata at the top level and then stamps the
// platform's own fields over it, so a trigger correlates with {{order_id}} the same
// way a data.* trigger reads a column, and a metadata key can never shadow
// external_id/status/amount. Best-effort: the money row is already committed, so a
// failure to enrich or publish must not fail the webhook and invite a redelivery
// that double-applies.
func publishPaymentEvent(env WebhookEnv, externalID, status string) {
	evType := paymentEventFor(status)
	if evType == "" || env.Bus == nil {
		return
	}
	payload := map[string]any{}

	var amount, currency, description, metadata sql.NullString
	if err := env.DB.QueryRow(
		"SELECT amount, currency, description, metadata FROM of_payments WHERE external_id = ?",
		externalID,
	).Scan(&amount, &currency, &description, &metadata); err == nil {
		if metadata.Valid && metadata.String != "" {
			var meta map[string]any
			if json.Unmarshal([]byte(metadata.String), &meta) == nil {
				for k, v := range meta {
					payload[k] = v
				}
			}
		}
		// Strings, not numbers: these are read by trigger {{...}} templating and by
		// the bot's status line, both of which want text.
		payload["amount"] = amount.String
		payload["currency"] = currency.String
		payload["description"] = description.String
	}
	payload["external_id"] = externalID
	payload["status"] = status
	if evType == events.EventPaymentFailed {
		payload["reason"] = status
	}
	env.Bus.Publish(events.NewEvent(evType, env.ProjectID, payload))
}

// FlowCheckoutPrefix starts the external id of every checkout a user-flow replay
// opens (see tools/payments): the replay pays on the platform's stub checkout page
// instead of the provider's, so a flow can walk plan -> checkout -> paid without
// real money or a page on another domain.
const FlowCheckoutPrefix = "of_flowtest_cs_"

// SettleFlowCheckout settles a replay's stub checkout exactly as a verified
// provider webhook settles a real one: the pending payment row turns paid (or
// canceled) and payment.completed / payment.failed is raised for fulfillment. Only
// ids the stub minted qualify, and only while they are pending; the caller also
// checks a replay of the project is running. Such a payment never involved money,
// and the replay's snapshot restore removes it.
func SettleFlowCheckout(env WebhookEnv, externalID string, paid bool) error {
	if !strings.HasPrefix(externalID, FlowCheckoutPrefix) {
		return refusef("not a test checkout")
	}
	status := "canceled"
	if paid {
		status = "paid"
	}
	res, err := env.DB.Exec(
		"UPDATE of_payments SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE external_id = ? AND status = 'pending'",
		status, externalID,
	)
	if err != nil {
		return fmt.Errorf("updating payment status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return refusef("no pending test checkout %q", externalID)
	}
	publishPaymentEvent(env, externalID, status)
	return nil
}

// FlowCheckout is what a replay's stub checkout page shows and where it sends the
// buyer afterwards.
type FlowCheckout struct {
	ProjectID             int
	Amount                int
	Currency, Description string
	SuccessURL, CancelURL string
}

var (
	testCheckoutsMu sync.Mutex
	testCheckouts   = map[string]FlowCheckout{}
)

// RememberFlowCheckout records a stub checkout the payments tool opened.
func RememberFlowCheckout(externalID string, c FlowCheckout) {
	testCheckoutsMu.Lock()
	defer testCheckoutsMu.Unlock()
	if len(testCheckouts) > 1000 { // replays are short; never let this grow unbounded
		clear(testCheckouts)
	}
	testCheckouts[externalID] = c
}

// LookupFlowCheckout returns a stub checkout by its external id.
func LookupFlowCheckout(externalID string) (FlowCheckout, bool) {
	testCheckoutsMu.Lock()
	defer testCheckoutsMu.Unlock()
	c, ok := testCheckouts[externalID]
	return c, ok
}
