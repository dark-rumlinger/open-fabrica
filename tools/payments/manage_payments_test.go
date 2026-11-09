package payments

import (
	"context"
	"strings"
	"testing"

	"openfabrica/tools"
)

// TestPaymentConfigure_RequiresWebhookSecret guards the WS2 fix: the
// webhook_secret_name arg is marked ,required, so an omitted value is rejected
// by the per-action schema validator (a structured missing_arg) before the
// handler runs - not as the handler's business-logic error. The desc, the
// schema, and the handler now agree that the secret is mandatory.
func TestPaymentConfigure_RequiresWebhookSecret(t *testing.T) {
	def, err := tools.DefaultRegistry().Get("manage_payments")
	if err != nil {
		t.Fatalf("Get(manage_payments): %v", err)
	}
	res, err := def.Execute(&tools.ToolContext{Ctx: context.Background()}, map[string]any{
		"action":        "configure",
		"provider_name": "stripe",
		"provider_type": "stripe",
		"currency":      "usd",
		// webhook_secret_name deliberately omitted
	})
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if res.Success {
		t.Fatalf("expected failure when webhook_secret_name is omitted, got success: %+v", res)
	}
	if res.Code != "missing_arg" {
		t.Fatalf("expected code %q (schema-level), got %q (error: %s)", "missing_arg", res.Code, res.Error)
	}
	if !strings.Contains(res.Error, "webhook_secret_name") {
		t.Fatalf("expected the error to name webhook_secret_name, got: %s", res.Error)
	}
}

// TestSubscriptionPlanRefusesNonStripe pins recurring billing to Stripe: only Stripe
// subscription events are reconciled, so a plan on PayPal would sell a subscription
// whose fulfilment never fires.
func TestSubscriptionPlanRefusesNonStripe(t *testing.T) {
	ctx, db, _ := newPaymentsCtx(t)
	if _, err := db.Exec(`UPDATE of_payment_config SET provider_type = 'paypal' WHERE id = 1`); err != nil {
		t.Fatalf("set provider type: %v", err)
	}
	def, err := tools.DefaultRegistry().Get("manage_subscription_plans")
	if err != nil {
		t.Fatalf("Get(manage_subscription_plans): %v", err)
	}
	res, err := def.Execute(ctx, map[string]any{
		"action":   "create",
		"name":     "Pro",
		"amount":   1500,
		"interval": "month",
	})
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if res.Success {
		t.Fatalf("a PayPal subscription plan was accepted: %+v", res)
	}
	if !strings.Contains(res.Error, "stripe") {
		t.Fatalf("the refusal should name the supported provider, got: %s", res.Error)
	}
}
