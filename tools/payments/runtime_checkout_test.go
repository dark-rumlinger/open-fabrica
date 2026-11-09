package payments

import (
	"encoding/json"
	"strings"
	"testing"

	"openfabrica/internal/funcexec"
	"openfabrica/internal/replayoutbox"
	"openfabrica/tools/tooltest"
)

// A server function opens checkout through ctx.checkout and gets the same result, and
// the same payments row, the create_checkout tool produces - the row the payment
// webhook later reconciles into payment.completed. Run under a user-flow replay so the
// session lands on the platform's stub checkout instead of a real provider.
func TestRuntimeCheckoutFromServerFunction(t *testing.T) {
	h := tooltest.New(t)
	db := h.Project.Writer()
	if _, err := db.Exec(`INSERT INTO of_payment_config (id, provider_name, provider_type, currency, webhook_secret_name) VALUES (1, 'acme', 'stripe', 'usd', 'whsec')`); err != nil {
		t.Fatalf("seed payment config: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO of_service_providers (name, base_url, auth_type, is_enabled) VALUES ('acme', 'https://api.stripe.com', 'bearer', 1)`); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	const project = 4242
	end := replayoutbox.Begin(project)
	defer end()

	rt := funcexec.Runtime{Reader: db, Writer: db, Enc: h.Env.Encryptor, ProjectID: project}
	code := `function handler(req, ctx) {
  const c = ctx.checkout({ amount: 1999, description: "Poster", success_url: "/thanks", cancel_url: "/shop", metadata: { order_id: 7 } });
  return { checkout_url: c.checkout_url, payment_id: c.payment_id };
}`
	resp, err := funcexec.Execute(rt, funcexec.Spec{Code: code}, map[string]any{"method": "POST"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("decode body %s: %v", resp.Body, err)
	}
	if url, _ := body["checkout_url"].(string); !strings.HasPrefix(url, "/__of/checkout/") {
		t.Fatalf("expected the replay stub checkout URL, got %s", resp.Body)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM of_payments WHERE amount = 1999 AND status = 'pending'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ctx.checkout must record one pending payment row, got %d (err %v)", n, err)
	}
}
