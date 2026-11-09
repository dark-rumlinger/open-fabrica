package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"openfabrica/events"
	"openfabrica/internal/dbh"
	"openfabrica/security"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// testKey is a fixed 32-byte (64 hex char) AES key for the test encryptor.
const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func stripeSig(secret, body string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, body)))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// ---------------------------------------------------------------------------
// handle_webhook end to end: signature -> reconcile, with idempotency and
// terminal-state ordering guarantees against an in-memory DB + real encryptor.
// ---------------------------------------------------------------------------

func newPaymentsCtx(t *testing.T) (*tools.ToolContext, dbh.Handle, string) {
	t.Helper()
	db := tooltest.ProjectDB(t)

	enc, err := security.NewEncryptor(testKey)
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}

	const secret = "whsec_live"
	ciphertext, err := enc.Encrypt(secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// The webhook signing secret must be stored encrypted, never in plaintext.
	if ciphertext == secret {
		t.Fatal("webhook secret stored as plaintext")
	}
	if _, err := db.Exec(`INSERT INTO of_secrets (name, value_encrypted) VALUES ('whsec', ?)`, ciphertext); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	db.Exec(`INSERT INTO of_payment_config (id, provider_name, provider_type, currency, webhook_secret_name)
	         VALUES (1, 'acme', 'stripe', 'usd', 'whsec')`)

	ctx := &tools.ToolContext{Ctx: context.Background(), DB: db, Encryptor: enc}
	return ctx, db, secret
}

// TestHandleWebhook_RaisesPaymentCompleted pins that handle_webhook RAISES a
// payment.completed event, not just reconciles the row: fulfillment hangs off that
// event, so if it raised nothing every trigger the model wired to it would sit
// silent while the money arrived - a build that ships green and delivers nothing.
func TestHandleWebhook_RaisesPaymentCompleted(t *testing.T) {
	ctx, db, secret := newPaymentsCtx(t)
	bus := events.NewBus()
	ctx.Bus = bus
	ctx.ProjectID = 7

	got := make(chan events.Event, 4)
	bus.Subscribe(events.EventPaymentCompleted, func(e events.Event) { got <- e })

	db.Exec(`INSERT INTO of_payments (external_id, amount, currency, status, description, metadata)
	         VALUES ('pi_evt', 1999, 'usd', 'pending', 'Pro plan', '{"order_id":"A-1"}')`)
	body := `{"type":"checkout.session.completed","data":{"object":{"id":"pi_evt","payment_status":"paid"}}}`
	if res, err := (&PaymentHandleWebhook{WebhookBody: body, WebhookSignature: stripeSig(secret, body, time.Now().Unix())}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("run: err=%v res=%+v", err, res)
	}

	select {
	case e := <-got:
		if e.ProjectID != 7 {
			t.Errorf("event must carry the project id, got %d", e.ProjectID)
		}
		// external_id is the correlation key that actually exists in the schema.
		if e.Payload["external_id"] != "pi_evt" {
			t.Errorf("payload should carry external_id, got %+v", e.Payload)
		}
		// The checkout metadata spreads at the top level, so a trigger reads
		// {{order_id}} the same way a data.* trigger reads a column.
		if e.Payload["order_id"] != "A-1" {
			t.Errorf("checkout metadata should spread at the top level, got %+v", e.Payload)
		}
		if e.Payload["amount"] != "1999" {
			t.Errorf("payload should carry the amount as text, got %+v", e.Payload["amount"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handle_webhook reconciled the payment but raised no payment.completed: fulfillment can never fire")
	}
}

// TestHandleWebhook_PaymentIdempotent: a paid event reconciles the row, and a
// replay of the same event leaves exactly one row in the same state.
func TestHandleWebhook_PaymentIdempotent(t *testing.T) {
	ctx, db, secret := newPaymentsCtx(t)
	db.Exec(`INSERT INTO of_payments (external_id, amount, currency, status) VALUES ('pi_123', 1999, 'usd', 'pending')`)

	body := `{"type":"checkout.session.completed","data":{"object":{"id":"pi_123","payment_status":"paid"}}}`
	sig := stripeSig(secret, body, time.Now().Unix())

	for i := 0; i < 2; i++ {
		res, err := (&PaymentHandleWebhook{WebhookBody: body, WebhookSignature: sig}).Run(ctx)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !res.Success {
			t.Fatalf("run %d failed: %s", i, res.Error)
		}
	}

	var count int
	var status string
	db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(status),'') FROM of_payments WHERE external_id='pi_123'`).Scan(&count, &status)
	if count != 1 {
		t.Errorf("payment row count = %d after replay; want 1 (idempotent)", count)
	}
	if status != "paid" {
		t.Errorf("payment status = %q; want paid", status)
	}
}

// TestHandleWebhook_SubscriptionTerminalCancel: pending->active->canceled, and a
// late/replayed renewal must NOT resurrect a canceled subscription.
func TestHandleWebhook_SubscriptionTerminalCancel(t *testing.T) {
	ctx, db, secret := newPaymentsCtx(t)
	db.Exec(`INSERT INTO of_subscriptions (external_id, status) VALUES ('sub_1', 'active')`)

	sign := func(body string) string { return stripeSig(secret, body, time.Now().Unix()) }
	run := func(body string) {
		res, err := (&PaymentHandleWebhook{WebhookBody: body, WebhookSignature: sign(body)}).Run(ctx)
		if err != nil || !res.Success {
			t.Fatalf("webhook run failed: err=%v res=%+v", err, res)
		}
	}

	// Cancel (terminal).
	run(`{"type":"customer.subscription.deleted","data":{"object":{"id":"sub_1"}}}`)
	var status string
	var canceledAt sql.NullString
	db.QueryRow(`SELECT status, canceled_at FROM of_subscriptions WHERE external_id='sub_1'`).Scan(&status, &canceledAt)
	if status != "canceled" {
		t.Fatalf("status after cancel = %q; want canceled", status)
	}
	if !canceledAt.Valid {
		t.Error("canceled_at not set on cancel")
	}

	// Out-of-order renewal must not revert to active.
	run(`{"type":"invoice.paid","data":{"object":{"subscription":"sub_1"}}}`)
	db.QueryRow(`SELECT status FROM of_subscriptions WHERE external_id='sub_1'`).Scan(&status)
	if status != "canceled" {
		t.Errorf("status after replayed renewal = %q; want canceled (terminal guard)", status)
	}
}

// TestHandleWebhook_RejectsBadSignature: a forged signature must be refused and
// must not mutate any row.
func TestHandleWebhook_RejectsBadSignature(t *testing.T) {
	ctx, db, _ := newPaymentsCtx(t)
	db.Exec(`INSERT INTO of_payments (external_id, amount, currency, status) VALUES ('pi_x', 500, 'usd', 'pending')`)

	body := `{"type":"checkout.session.completed","data":{"object":{"id":"pi_x","payment_status":"paid"}}}`
	res, err := (&PaymentHandleWebhook{WebhookBody: body, WebhookSignature: stripeSig("WRONG", body, time.Now().Unix())}).Run(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Success {
		t.Fatal("forged signature accepted")
	}
	var status string
	db.QueryRow(`SELECT status FROM of_payments WHERE external_id='pi_x'`).Scan(&status)
	if status != "pending" {
		t.Errorf("payment status = %q after forged webhook; want unchanged (pending)", status)
	}
}
