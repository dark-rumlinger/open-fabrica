package billing

import (
	"testing"
)

func TestPaymentRefundTransitionAllowedAndTerminal(t *testing.T) {
	rig := newReconcileRig(t)
	if _, err := rig.env.DB.Exec(`INSERT INTO of_payments (amount, currency, status, external_id) VALUES (1000, 'usd', 'paid', 'pi_ref_1')`); err != nil {
		t.Fatal(err)
	}

	// paid -> refunded must succeed.
	out, err := rig.send(t, stripeEvent("evt_refund_1", "charge.refunded", map[string]any{
		"id":             "ch_1",
		"payment_intent": "pi_ref_1",
	}))
	if err != nil {
		t.Fatalf("refund reconcile failed: %v", err)
	}
	if out.Kind != WebhookPayment || out.Status != "refunded" {
		t.Fatalf("want WebhookPayment/refunded, got %+v", out)
	}
	if got := rig.paymentStatus(t, "pi_ref_1"); got != "refunded" {
		t.Fatalf("want DB status refunded, got %q", got)
	}

	// refunded -> paid (late duplicate) must be ignored.
	out2, err := rig.send(t, stripeEvent("evt_late_1", "checkout.session.async_payment_succeeded", map[string]any{
		"id": "pi_ref_1",
	}))
	if err != nil {
		t.Fatalf("late paid reconcile failed: %v", err)
	}
	if out2.Kind != WebhookIgnored {
		t.Fatalf("want WebhookIgnored for refunded->paid, got %+v", out2)
	}
}

func TestSubscriptionIgnoresOutOfOrderOlderEvent(t *testing.T) {
	rig := newReconcileRig(t)
	if _, err := rig.env.DB.Exec(`INSERT INTO of_subscriptions (external_id, status, last_event_created) VALUES ('sub_ord_1', 'active', 2000)`); err != nil {
		t.Fatal(err)
	}
	tx, err := rig.env.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	matched, shouldPublish, err := applySubscriptionChange(tx, subscriptionChange{
		externalID:   "sub_ord_1",
		status:       "past_due",
		eventCreated: 1500, // older than 2000
	}, nil)
	if err != nil {
		t.Fatalf("applySubscriptionChange: %v", err)
	}
	if !matched || shouldPublish {
		t.Fatalf("expected stale event to be matched=true, shouldPublish=false; got matched=%v publish=%v", matched, shouldPublish)
	}
	var status string
	if err := tx.QueryRow(`SELECT status FROM of_subscriptions WHERE external_id = 'sub_ord_1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("stale event regressed status to %q", status)
	}
}
