package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"openfabrica/events"
	"openfabrica/internal/dbtest"
	"openfabrica/security"
)

const reconcileSecret = "whsec_reconcile"

type reconcileRig struct {
	env WebhookEnv
	got *[]events.Event
}

// newReconcileRig is a migrated project DB configured for Stripe with a stored
// signing secret, plus a bus that records every event.
func newReconcileRig(t *testing.T) reconcileRig {
	t.Helper()
	db := dbtest.ProjectWriterDB(t).ToolHandle(context.Background())
	enc, err := security.NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := enc.Encrypt(reconcileSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_payment_config (id, provider_name, provider_type, webhook_secret_name) VALUES (1, 'stripe', 'stripe', 'wh')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO of_secrets (name, value_encrypted) VALUES ('wh', ?)`, sealed); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	var got []events.Event
	bus.SubscribeAll(func(e events.Event) { got = append(got, e) })
	return reconcileRig{env: WebhookEnv{DB: db, Encryptor: enc, Bus: bus, ProjectID: 7}, got: &got}
}

func (r reconcileRig) send(t *testing.T, evt map[string]any) (*WebhookOutcome, error) {
	t.Helper()
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	return ReconcileWebhook(r.env, body, stripeSig(reconcileSecret, body, time.Now().Unix()))
}

func stripeEvent(id, typ string, obj map[string]any) map[string]any {
	return map[string]any{"id": id, "type": typ, "data": map[string]any{"object": obj}}
}

func (r reconcileRig) paymentStatus(t *testing.T, externalID string) string {
	t.Helper()
	var s string
	if err := r.env.DB.QueryRow(`SELECT status FROM of_payments WHERE external_id = ?`, externalID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r reconcileRig) addPayment(t *testing.T, externalID string) {
	t.Helper()
	if _, err := r.env.DB.Exec(`INSERT INTO of_payments (external_id, amount, currency, status, metadata) VALUES (?, 1000, 'usd', 'pending', '{"order_id":"9"}')`, externalID); err != nil {
		t.Fatal(err)
	}
}

func (r reconcileRig) addSubscription(t *testing.T, externalID, status string) {
	t.Helper()
	if _, err := r.env.DB.Exec(`INSERT INTO of_subscriptions (external_id, status, metadata) VALUES (?, ?, '{"user_ref":"u1"}')`, externalID, status); err != nil {
		t.Fatal(err)
	}
}

func (r reconcileRig) subscription(t *testing.T, externalID string) string {
	t.Helper()
	var s string
	if err := r.env.DB.QueryRow(`SELECT status FROM of_subscriptions WHERE external_id = ?`, externalID).Scan(&s); err != nil {
		t.Fatalf("subscription %q: %v", externalID, err)
	}
	return s
}

func TestReconcile_PaidCompletionRaisesOnce(t *testing.T) {
	r := newReconcileRig(t)
	r.addPayment(t, "cs_1")
	evt := stripeEvent("evt_1", "checkout.session.completed", map[string]any{"id": "cs_1", "payment_status": "paid"})

	out, err := r.send(t, evt)
	if err != nil || out.Kind != WebhookPayment {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if r.paymentStatus(t, "cs_1") != "paid" {
		t.Fatal("payment not marked paid")
	}
	if out, _ = r.send(t, evt); out.Kind != WebhookDuplicate {
		t.Fatalf("replay kind = %q", out.Kind)
	}
	if len(*r.got) != 1 || (*r.got)[0].Type != events.EventPaymentCompleted {
		t.Fatalf("events = %+v", *r.got)
	}
}

func TestReconcile_UnpaidCompletionWaitsForAsyncResult(t *testing.T) {
	r := newReconcileRig(t)
	r.addPayment(t, "cs_2")

	out, err := r.send(t, stripeEvent("evt_a", "checkout.session.completed", map[string]any{"id": "cs_2", "payment_status": "unpaid"}))
	if err != nil || out.Kind != WebhookIgnored {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if r.paymentStatus(t, "cs_2") != "pending" || len(*r.got) != 0 {
		t.Fatalf("unpaid completion must leave the payment pending and silent, got %q %v", r.paymentStatus(t, "cs_2"), *r.got)
	}
	if _, err := r.send(t, stripeEvent("evt_b", "checkout.session.async_payment_succeeded", map[string]any{"id": "cs_2", "payment_status": "paid"})); err != nil {
		t.Fatal(err)
	}
	if r.paymentStatus(t, "cs_2") != "paid" || len(*r.got) != 1 || (*r.got)[0].Type != events.EventPaymentCompleted {
		t.Fatalf("async success must settle: %q %v", r.paymentStatus(t, "cs_2"), *r.got)
	}
}

func TestReconcile_UnrelatedEventIsAcknowledgedWithoutClaim(t *testing.T) {
	r := newReconcileRig(t)
	out, err := r.send(t, stripeEvent("evt_c", "charge.succeeded", map[string]any{"id": "ch_1"}))
	if err != nil || out.Kind != WebhookIgnored {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	var n int
	_ = r.env.DB.QueryRow(`SELECT COUNT(*) FROM of_processed_webhooks`).Scan(&n)
	if n != 0 {
		t.Fatalf("an ignored event must not consume an idempotency slot, got %d", n)
	}
}

func TestReconcile_PaidIsTerminal(t *testing.T) {
	r := newReconcileRig(t)
	r.addPayment(t, "cs_3")
	if _, err := r.send(t, stripeEvent("evt_d", "checkout.session.completed", map[string]any{"id": "cs_3", "payment_status": "paid"})); err != nil {
		t.Fatal(err)
	}
	out, err := r.send(t, stripeEvent("evt_e", "checkout.session.expired", map[string]any{"id": "cs_3"}))
	if err != nil || out.Kind != WebhookIgnored {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if r.paymentStatus(t, "cs_3") != "paid" || len(*r.got) != 1 {
		t.Fatalf("a settled payment must not regress: %q %v", r.paymentStatus(t, "cs_3"), *r.got)
	}
}

func TestReconcile_UnknownPaymentRaisesNothing(t *testing.T) {
	r := newReconcileRig(t)
	out, err := r.send(t, stripeEvent("evt_f", "checkout.session.completed", map[string]any{"id": "cs_missing", "payment_status": "paid"}))
	if err != nil || out.Kind != WebhookIgnored {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(*r.got) != 0 {
		t.Fatalf("no event for an unknown payment, got %v", *r.got)
	}
}

func TestReconcile_ExpiredSessionFails(t *testing.T) {
	r := newReconcileRig(t)
	r.addPayment(t, "cs_4")
	if _, err := r.send(t, stripeEvent("evt_g", "checkout.session.expired", map[string]any{"id": "cs_4"})); err != nil {
		t.Fatal(err)
	}
	if r.paymentStatus(t, "cs_4") != "expired" || len(*r.got) != 1 || (*r.got)[0].Type != events.EventPaymentFailed {
		t.Fatalf("expired must raise payment.failed: %q %v", r.paymentStatus(t, "cs_4"), *r.got)
	}
}

func TestReconcile_SubscriptionCheckoutLinksSessionToSubscription(t *testing.T) {
	r := newReconcileRig(t)
	r.addSubscription(t, "cs_sub", "pending")

	out, err := r.send(t, stripeEvent("evt_h", "checkout.session.completed", map[string]any{
		"id": "cs_sub", "mode": "subscription", "subscription": "sub_1", "payment_status": "paid",
	}))
	if err != nil || out.Kind != WebhookSubscription || out.ExternalID != "sub_1" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if r.subscription(t, "sub_1") != "active" {
		t.Fatal("linked subscription must be active")
	}
	if len(*r.got) != 1 || (*r.got)[0].Type != events.EventPaymentCompleted {
		t.Fatalf("events = %+v", *r.got)
	}
	payload := (*r.got)[0].Payload
	if payload["subscription_id"] == nil || payload["external_id"] != "sub_1" || payload["user_ref"] != "u1" {
		t.Fatalf("payload = %v", payload)
	}

	// The first invoice follows the checkout: same payment, no second event.
	first := stripeEvent("evt_i", "invoice.paid", map[string]any{"subscription": "sub_1", "billing_reason": "subscription_create"})
	if _, err := r.send(t, first); err != nil {
		t.Fatal(err)
	}
	if len(*r.got) != 1 {
		t.Fatalf("first invoice must not raise a second event, got %d", len(*r.got))
	}
	// A later cycle is a real renewal.
	if _, err := r.send(t, stripeEvent("evt_j", "invoice.paid", map[string]any{"subscription": "sub_1", "billing_reason": "subscription_cycle"})); err != nil {
		t.Fatal(err)
	}
	if len(*r.got) != 2 {
		t.Fatalf("renewal must raise an event, got %d", len(*r.got))
	}
}

func TestReconcile_DelayedSubscriptionActivatesOnFirstInvoice(t *testing.T) {
	r := newReconcileRig(t)
	r.addSubscription(t, "cs_late", "pending")
	if _, err := r.send(t, stripeEvent("evt_k", "checkout.session.completed", map[string]any{
		"id": "cs_late", "mode": "subscription", "subscription": "sub_2", "payment_status": "unpaid",
	})); err != nil {
		t.Fatal(err)
	}
	if r.subscription(t, "sub_2") != "pending" || len(*r.got) != 0 {
		t.Fatal("unpaid checkout links the subscription but stays pending and silent")
	}
	if _, err := r.send(t, stripeEvent("evt_l", "invoice.paid", map[string]any{"subscription": "sub_2", "billing_reason": "subscription_create"})); err != nil {
		t.Fatal(err)
	}
	if r.subscription(t, "sub_2") != "active" || len(*r.got) != 1 {
		t.Fatalf("first invoice must activate and raise once: %q %d", r.subscription(t, "sub_2"), len(*r.got))
	}
}

func TestReconcile_SubscriptionFailureAndCancel(t *testing.T) {
	r := newReconcileRig(t)
	r.addSubscription(t, "sub_3", "active")

	if _, err := r.send(t, stripeEvent("evt_m", "invoice.payment_failed", map[string]any{"subscription": "sub_3"})); err != nil {
		t.Fatal(err)
	}
	if r.subscription(t, "sub_3") != "past_due" || len(*r.got) != 1 || (*r.got)[0].Type != events.EventPaymentFailed {
		t.Fatalf("payment failure must mark past_due and raise failed: %q %v", r.subscription(t, "sub_3"), *r.got)
	}
	if _, err := r.send(t, stripeEvent("evt_n", "customer.subscription.deleted", map[string]any{"id": "sub_3"})); err != nil {
		t.Fatal(err)
	}
	if r.subscription(t, "sub_3") != "canceled" {
		t.Fatal("deleted subscription must be canceled")
	}
	// Canceled is terminal.
	if _, err := r.send(t, stripeEvent("evt_o", "invoice.paid", map[string]any{"subscription": "sub_3", "billing_reason": "subscription_cycle"})); err != nil {
		t.Fatal(err)
	}
	if r.subscription(t, "sub_3") != "canceled" {
		t.Fatal("a late invoice must not revive a canceled subscription")
	}
}

func TestReconcile_UnknownSubscriptionRaisesNothing(t *testing.T) {
	r := newReconcileRig(t)
	out, err := r.send(t, stripeEvent("evt_p", "invoice.paid", map[string]any{"subscription": "sub_ghost", "billing_reason": "subscription_cycle"}))
	if err != nil || out.Kind != WebhookIgnored {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(*r.got) != 0 {
		t.Fatal(fmt.Sprint("unexpected events: ", *r.got))
	}
}
