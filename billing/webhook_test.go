package billing

import (
	"testing"

	"openfabrica/events"
)

// TestPaymentEventFor_UnsettledRaisesNothing pins the other half: a trigger fires once
// per event, so announcing an outcome that has not happened is worse than announcing
// it late. A still-moving status must stay silent and wait for the settled webhook.
func TestPaymentEventFor_UnsettledRaisesNothing(t *testing.T) {
	for _, status := range []string{"unpaid", "pending", "open"} {
		if ev := paymentEventFor(status); ev != "" {
			t.Errorf("%q has not settled and must raise nothing, got %q", status, ev)
		}
	}
	if ev := paymentEventFor("paid"); ev != events.EventPaymentCompleted {
		t.Errorf("paid must raise payment.completed, got %q", ev)
	}
}
