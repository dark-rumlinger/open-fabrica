package billing

import (
	"context"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/internal/dbtest"
)

// newIdempotencyDB is a migrated project database, so of_processed_webhooks is the
// production table.
func newIdempotencyDB(t *testing.T) dbh.Handle {
	t.Helper()
	return dbtest.ProjectWriterDB(t).ToolHandle(context.Background())
}

// TestClaimWebhookEventIsIdempotent pins the core dedup guarantee: the first
// claim of an event wins, a replay of the same event loses, and a released claim
// (processing failed) can be reprocessed.
func TestClaimWebhookEventIsIdempotent(t *testing.T) {
	db := newIdempotencyDB(t)
	key := "stripe:evt_123"

	if ok, err := claimWebhookEvent(db, key); err != nil || !ok {
		t.Fatalf("first claim should win: ok=%v err=%v", ok, err)
	}
	if ok, err := claimWebhookEvent(db, key); err != nil || ok {
		t.Fatalf("replay of same key must lose: ok=%v err=%v", ok, err)
	}
	if ok, err := claimWebhookEvent(db, "stripe:evt_456"); err != nil || !ok {
		t.Fatalf("distinct key should win: ok=%v err=%v", ok, err)
	}
	releaseWebhookEvent(db, key)
	if ok, err := claimWebhookEvent(db, key); err != nil || !ok {
		t.Fatalf("claim after release should win again: ok=%v err=%v", ok, err)
	}
}

// TestWebhookEventKey pins key derivation: prefer the provider event id, else a
// body hash, always provider-scoped.
func TestWebhookEventKey(t *testing.T) {
	if got := webhookEventKey("stripe", `{"id":"evt_1"}`, map[string]any{"id": "evt_1"}); got != "stripe:evt_1" {
		t.Errorf("id-based key = %q, want stripe:evt_1", got)
	}
	k1 := webhookEventKey("mollie", `{"x":1}`, map[string]any{})
	k2 := webhookEventKey("mollie", `{"x":1}`, map[string]any{})
	k3 := webhookEventKey("mollie", `{"x":2}`, map[string]any{})
	if k1 != k2 {
		t.Errorf("identical no-id bodies should share a key: %q vs %q", k1, k2)
	}
	if k1 == k3 {
		t.Errorf("different bodies should not share a key: %q == %q", k1, k3)
	}
	if got := webhookEventKey("paypal", `{}`, map[string]any{"id": "WH-9"}); got != "paypal:WH-9" {
		t.Errorf("paypal id key = %q, want paypal:WH-9", got)
	}
}
