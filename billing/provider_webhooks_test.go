package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func stripeSig(secret, body string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, body)))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func genericSig(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Signature verification - the gate that stops forged "paid" events.
// ---------------------------------------------------------------------------

func TestVerifyWebhookSignature_Stripe(t *testing.T) {
	const secret = "whsec_test"
	body := `{"type":"checkout.session.completed"}`
	now := time.Now().Unix()

	if !verifyWebhookSignature("stripe", body, stripeSig(secret, body, now), secret) {
		t.Error("valid fresh stripe signature rejected")
	}
	// Tampered body must fail.
	if verifyWebhookSignature("stripe", body+" ", stripeSig(secret, body, now), secret) {
		t.Error("tampered body accepted")
	}
	// Wrong secret must fail.
	if verifyWebhookSignature("stripe", body, stripeSig("other", body, now), secret) {
		t.Error("signature from wrong secret accepted")
	}
	// Replay outside the tolerance window must fail.
	old := now - stripeReplayToleranceSeconds - 60
	if verifyWebhookSignature("stripe", body, stripeSig(secret, body, old), secret) {
		t.Error("stale (replayed) signature accepted")
	}
	// Missing envelope parts must fail.
	if verifyWebhookSignature("stripe", body, "v1=deadbeef", secret) {
		t.Error("signature with no timestamp accepted")
	}
}

func TestVerifyWebhookSignature_Generic(t *testing.T) {
	const secret = "shh"
	body := `{"id":"pay_1","status":"paid"}`
	if !verifyWebhookSignature("mollie", body, genericSig(secret, body), secret) {
		t.Error("valid generic signature rejected")
	}
	if verifyWebhookSignature("mollie", body, genericSig("nope", body), secret) {
		t.Error("generic signature from wrong secret accepted")
	}
}

// ---------------------------------------------------------------------------
// Webhook payload parsers - state mapping across providers.
// ---------------------------------------------------------------------------

func TestParseWebhookStatus(t *testing.T) {
	stripe := map[string]any{
		"type": "checkout.session.completed",
		"data": map[string]any{
			"object": map[string]any{"id": "cs_1", "payment_status": "paid"},
		},
	}
	if id, st := parseWebhookStatus("stripe", stripe); id != "cs_1" || st != "paid" {
		t.Errorf("stripe parse = (%q,%q), want (cs_1,paid)", id, st)
	}
	paypal := map[string]any{
		"resource": map[string]any{"id": "PAY-9", "status": "COMPLETED"},
	}
	if id, st := parseWebhookStatus("paypal", paypal); id != "PAY-9" || st != "paid" {
		t.Errorf("paypal parse = (%q,%q), want (PAY-9,paid)", id, st)
	}
	mollie := map[string]any{"id": "tr_5", "status": "paid"}
	if id, st := parseWebhookStatus("mollie", mollie); id != "tr_5" || st != "paid" {
		t.Errorf("mollie parse = (%q,%q), want (tr_5,paid)", id, st)
	}
}

func TestParseSubscriptionWebhookEvent(t *testing.T) {
	paid := map[string]any{
		"type": "invoice.paid",
		"data": map[string]any{"object": map[string]any{"subscription": "sub_1"}},
	}
	if id, st := parseSubscriptionWebhookEvent("stripe", paid); id != "sub_1" || st != "active" {
		t.Errorf("invoice.paid parse = (%q,%q), want (sub_1,active)", id, st)
	}
	del := map[string]any{
		"type": "customer.subscription.deleted",
		"data": map[string]any{"object": map[string]any{"id": "sub_1"}},
	}
	if id, st := parseSubscriptionWebhookEvent("stripe", del); id != "sub_1" || st != "canceled" {
		t.Errorf("subscription.deleted parse = (%q,%q), want (sub_1,canceled)", id, st)
	}
}
