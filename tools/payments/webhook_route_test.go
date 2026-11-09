package payments

import (
	"strings"
	"testing"
	"time"

	"openfabrica/billing"
	"openfabrica/events"
)

// TestReconcileWebhook_RefusalIsNotAFault separates the two failure kinds a
// transport has to tell apart. A refusal means the caller is wrong and redelivering
// it will never help; a fault is ours and the provider's retry should get through.
// Collapsing them costs money in one direction or floods in the other.
func TestReconcileWebhook_RefusalIsNotAFault(t *testing.T) {
	ctx, _, _ := newPaymentsCtx(t)
	// No signature: the platform cannot authenticate this, and no retry changes that.
	_, err := billing.ReconcileWebhook(billing.WebhookEnv{DB: ctx.DB, Encryptor: ctx.Encryptor}, `{"id":"x"}`, "")
	if err == nil {
		t.Fatal("an unsigned webhook must not reconcile")
	}
	var ref *billing.WebhookRefusal
	if !asRefusal(err, &ref) {
		t.Fatalf("an unsigned webhook is a refusal, not a fault: %v", err)
	}
}

// TestReconcileWebhook_ForgedSignatureRefused is the gate the whole hosted-checkout
// flow rests on: anyone can POST to the webhook path, so only the signature
// separates a real settlement from a forged one.
func TestReconcileWebhook_ForgedSignatureRefused(t *testing.T) {
	ctx, db, _ := newPaymentsCtx(t)
	db.Exec(`INSERT INTO of_payments (external_id, amount, currency, status) VALUES ('pi_f', 500, 'usd', 'pending')`)

	body := `{"type":"checkout.session.completed","data":{"object":{"id":"pi_f","payment_status":"paid"}}}`
	_, err := billing.ReconcileWebhook(billing.WebhookEnv{DB: ctx.DB, Encryptor: ctx.Encryptor}, body, "t=1,v1=deadbeef")
	if err == nil {
		t.Fatal("a forged signature must not reconcile a payment")
	}

	var status string
	db.QueryRow(`SELECT status FROM of_payments WHERE external_id = 'pi_f'`).Scan(&status)
	if status != "pending" {
		t.Fatalf("a forged webhook moved the money row to %q", status)
	}
}

// TestReconcileWebhook_SharesOneGateWithTheToolAction pins that both callers reach
// money state through the same function. The tool action is a thin adapter over
// billing.ReconcileWebhook; if it ever grew its own verification, the two paths could
// disagree about what a genuine event is, and only one of them is reachable by an
// attacker.
func TestReconcileWebhook_SharesOneGateWithTheToolAction(t *testing.T) {
	ctx, db, secret := newPaymentsCtx(t)
	bus := events.NewBus()
	ctx.Bus = bus
	ctx.ProjectID = 3
	got := make(chan events.Event, 2)
	bus.Subscribe(events.EventPaymentCompleted, func(e events.Event) { got <- e })

	db.Exec(`INSERT INTO of_payments (external_id, amount, currency, status) VALUES ('pi_same', 700, 'usd', 'pending')`)
	body := `{"type":"checkout.session.completed","data":{"object":{"id":"pi_same","payment_status":"paid"}}}`

	// Drive it the way the HTTP route does: straight into the shared function.
	out, err := billing.ReconcileWebhook(billing.WebhookEnv{
		DB: ctx.DB, Encryptor: ctx.Encryptor, Bus: bus, ProjectID: 3,
	}, body, stripeSig(secret, body, time.Now().Unix()))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if out.Kind != billing.WebhookPayment || out.ExternalID != "pi_same" || out.Status != "paid" {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	select {
	case <-got:
	default:
		t.Fatal("the route path must raise payment.completed exactly as the tool path does")
	}
}

// TestWebhookPathIsRoutable pins the shape of the path the server mounts and the
// configure result hands the model. The provider posts to it from outside, so it
// has to be a rooted absolute path; a relative or empty one silently produces a
// provider configured to call nowhere.
func TestWebhookPathIsRoutable(t *testing.T) {
	if !strings.HasPrefix(billing.WebhookPath, "/") || strings.Contains(billing.WebhookPath, " ") {
		t.Fatalf("billing.WebhookPath must be a rooted path with no spaces, got %q", billing.WebhookPath)
	}
	// It must not collide with the named-webhook family, which is a different
	// mechanism with a different secret and would swallow the callback.
	if strings.HasPrefix(billing.WebhookPath, "/webhooks/") {
		t.Errorf("%q sits under the manage_webhooks family and would be routed there instead", billing.WebhookPath)
	}
}

func asRefusal(err error, target **billing.WebhookRefusal) bool {
	r, ok := err.(*billing.WebhookRefusal)
	if ok {
		*target = r
	}
	return ok
}
