package billing

import (
	"context"
	"testing"

	"openfabrica/events"
	"openfabrica/internal/dbtest"
)

// A replay's stub checkout settles like a verified webhook - the row turns paid and
// payment.completed fires with the checkout's metadata - but only for ids the stub
// minted, and only once.
func TestSettleFlowCheckout(t *testing.T) {
	db := dbtest.ProjectWriterDB(t).ToolHandle(context.Background())
	id := FlowCheckoutPrefix + "abc"
	if _, err := db.Exec(`INSERT INTO of_payments (external_id, amount, currency, status, description, metadata, checkout_url)
		VALUES (?, 1999, 'eur', 'pending', 'Annual plan', '{"order_id":"42"}', '/__of/checkout/x'), ('cs_real', 500, 'eur', 'pending', '', '', '')`, id); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	var got []events.Event
	bus.SubscribeAll(func(e events.Event) { got = append(got, e) })
	env := WebhookEnv{DB: db, Bus: bus, ProjectID: 5}

	if err := SettleFlowCheckout(env, id, true); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM of_payments WHERE external_id = ?`, id).Scan(&status); err != nil || status != "paid" {
		t.Fatalf("status = %q %v", status, err)
	}
	if len(got) != 1 || got[0].Type != events.EventPaymentCompleted {
		t.Fatalf("events = %+v", got)
	}
	if SettleFlowCheckout(env, id, true) == nil {
		t.Fatal("a settled checkout cannot settle again")
	}
	if SettleFlowCheckout(env, "cs_real", true) == nil {
		t.Fatal("a real provider payment can never be settled this way")
	}
}
