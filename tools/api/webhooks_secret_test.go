package api

import (
	"strings"
	"testing"

	"openfabrica/tools/tooltest"
)

// A webhook's signing secret is returned to the caller once, at creation, and read
// back through get, but the database only ever holds ciphertext of it: a copy of the
// project database (a backup, a leaked file) must not carry usable signing keys.
func TestWebhookSecretIsStoredEncrypted(t *testing.T) {
	h := tooltest.New(t)

	created := h.MustOK("manage_webhooks", map[string]any{"action": "create", "name": "orders"})
	data, ok := created.Data.(map[string]any)
	if !ok {
		t.Fatalf("create returned %T, want a map", created.Data)
	}
	secret, _ := data["secret"].(string)
	if len(secret) < 32 {
		t.Fatalf("create returned a short or missing secret: %q", secret)
	}

	var stored string
	if err := h.DB().QueryRow(`SELECT secret_encrypted FROM of_webhooks WHERE name = 'orders'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || stored == secret || strings.Contains(stored, secret) {
		t.Fatalf("stored value %q is not ciphertext of the secret", stored)
	}
	if opened, err := h.Env.Encryptor.OpenOptional(stored); err != nil || opened != secret {
		t.Fatalf("stored value does not decrypt to the returned secret: %q, %v", opened, err)
	}

	got := h.MustOK("manage_webhooks", map[string]any{"action": "get", "name": "orders"})
	if gd, _ := got.Data.(map[string]any); gd["secret"] != secret {
		t.Errorf("get returned secret %v, want the one created", gd["secret"])
	}

	// Two webhooks never share ciphertext, even if a secret repeated: each is sealed
	// with its own nonce.
	h.MustOK("manage_webhooks", map[string]any{"action": "create", "name": "billing"})
	var n int
	if err := h.DB().QueryRow(`SELECT COUNT(DISTINCT secret_encrypted) FROM of_webhooks`).Scan(&n); err != nil || n != 2 {
		t.Errorf("distinct sealed secrets = %d (%v), want 2", n, err)
	}
}
