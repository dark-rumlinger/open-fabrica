package public

import (
	"context"
	"crypto/hmac"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/security"
)

// TestIncomingWebhookSignatureContract pins the HMAC format the incoming
// webhook handler verifies against: lowercase hex of HMAC-SHA256(body, secret),
// optionally prefixed with "sha256=". A correct signature must match and any
// tampering (body or secret) must not. This is the contract a forged unsigned
// request cannot satisfy.
func TestIncomingWebhookSignatureContract(t *testing.T) {
	secret := []byte("super-secret-signing-key")
	body := []byte(`{"event":"order.paid","amount":1999}`)

	good := "sha256=" + security.HMACSHA256Hex(secret, body)

	// Mirrors the handler's comparison in IncomingWebhook.
	verify := func(signature string, body, secret []byte) bool {
		sig := strings.TrimPrefix(signature, "sha256=")
		expected := security.HMACSHA256Hex(secret, body)
		return hmac.Equal([]byte(strings.ToLower(sig)), []byte(expected))
	}

	if !verify(good, body, secret) {
		t.Fatal("correctly signed payload failed verification")
	}
	// Uppercase hex is accepted; hex.DecodeString in the old handler was
	// case-insensitive and external signers rely on that.
	if !verify(strings.ToUpper(strings.TrimPrefix(good, "sha256=")), body, secret) {
		t.Fatal("uppercase hex signature failed verification")
	}
	if verify(good, []byte(`{"event":"order.paid","amount":999999}`), secret) {
		t.Fatal("tampered body passed verification")
	}
	if verify(good, body, []byte("wrong-secret")) {
		t.Fatal("wrong secret passed verification")
	}
	if verify("not-hex", body, secret) {
		t.Fatal("malformed signature passed verification")
	}
	if verify("", body, secret) {
		t.Fatal("empty signature passed verification")
	}
}

// TestWebhookEventPayloadFlattensBody pins the webhook.received payload shape the
// trigger-events contract documents: the parsed body's fields sit at the top
// level (so a trigger reads them with {{field}}), and webhook_id/webhook_name are
// platform metadata that a body field of the same name cannot shadow or spoof.
func TestWebhookEventPayloadFlattensBody(t *testing.T) {
	// A body field is lifted to the top level; metadata sits alongside it.
	p := webhookEventPayload(7, "stripe", []byte(`{"email":"a@b.com","amount":1999}`))
	if p["email"] != "a@b.com" {
		t.Errorf("body field not flattened: email=%v", p["email"])
	}
	if p["webhook_id"] != 7 || p["webhook_name"] != "stripe" {
		t.Errorf("metadata missing: webhook_id=%v webhook_name=%v", p["webhook_id"], p["webhook_name"])
	}

	// A body field colliding with metadata cannot override the platform values.
	spoof := webhookEventPayload(7, "stripe", []byte(`{"webhook_id":"evil","webhook_name":"evil"}`))
	if spoof["webhook_id"] != 7 || spoof["webhook_name"] != "stripe" {
		t.Errorf("body field spoofed metadata: webhook_id=%v webhook_name=%v", spoof["webhook_id"], spoof["webhook_name"])
	}

	// A non-JSON body contributes no fields but still carries the metadata.
	nonJSON := webhookEventPayload(7, "stripe", []byte("not json"))
	if nonJSON["webhook_id"] != 7 || nonJSON["webhook_name"] != "stripe" {
		t.Errorf("non-JSON body dropped metadata: %v", nonJSON)
	}
}

// A sender that supplies X-Webhook-Timestamp signs "<timestamp>.<body>" and is
// rejected outside the replay window; one that does not keeps the body-only form.
func TestWebhookSignedPayloadReplayWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"event":"order.paid"}`)

	got, err := webhookSignedPayload(body, "", now)
	if err != nil || string(got) != string(body) {
		t.Fatalf("no header: got %q, %v; want the bare body", got, err)
	}

	fresh := strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	got, err = webhookSignedPayload(body, fresh, now)
	if err != nil || string(got) != fresh+"."+string(body) {
		t.Fatalf("fresh timestamp: got %q, %v", got, err)
	}

	for name, ts := range map[string]string{
		"stale":      strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10),
		"far future": strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10),
		"not a time": "yesterday",
	} {
		if _, err := webhookSignedPayload(body, ts, now); err == nil {
			t.Errorf("%s timestamp was accepted", name)
		}
	}

	// The timestamp is part of what is signed: a captured signature does not carry
	// over to a different timestamp.
	secret := []byte("k")
	a, _ := webhookSignedPayload(body, fresh, now)
	b, _ := webhookSignedPayload(body, strconv.FormatInt(now.Unix(), 10), now)
	if security.HMACSHA256Hex(secret, a) == security.HMACSHA256Hex(secret, b) {
		t.Error("signature does not depend on the timestamp")
	}
}

// The incoming webhook handler verifies against the DECRYPTED secret: a correctly
// signed request is accepted, a wrong signature is refused, and a secret that cannot
// be decrypted (the key was replaced) or was never set refuses everything rather
// than letting anything unverified through.
func TestIncomingWebhookVerifiesAgainstTheEncryptedSecret(t *testing.T) {
	enc, err := security.NewEncryptor(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	pm := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(pm.CloseAll)
	pdb, err := pm.Create(1)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := enc.SealOptional("whsec-live")
	if err != nil {
		t.Fatal(err)
	}
	for name, stored := range map[string]string{"good": sealed, "empty": "", "broken": "not-ciphertext"} {
		if _, err := pdb.ExecWrite(`INSERT INTO of_webhooks (name, secret_encrypted, direction) VALUES (?, ?, 'incoming')`, name, stored); err != nil {
			t.Fatal(err)
		}
	}
	// An OUTGOING webhook's secret signs what we send; it must not authenticate a
	// request posted to the inbound route.
	if _, err := pdb.ExecWrite(`INSERT INTO of_webhooks (name, secret_encrypted, url, direction) VALUES ('outbound', ?, 'https://example.com/hook', 'outgoing')`, sealed); err != nil {
		t.Fatal(err)
	}
	h := &Handler{deps: &Deps{ProjectDBManager: pm, Encryptor: enc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}

	body := `{"event":"order.paid"}`
	call := func(name, secret string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/"+name, strings.NewReader(body))
		if secret != "" {
			req.Header.Set("X-Webhook-Signature", "sha256="+security.HMACSHA256Hex([]byte(secret), []byte(body)))
		}
		rc := chi.NewRouteContext()
		rc.URLParams.Add("name", name)
		ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
		req = req.WithContext(context.WithValue(ctx, projectContextKey, &models.Project{ID: 1}))
		rec := httptest.NewRecorder()
		h.IncomingWebhook(rec, req)
		return rec.Code
	}

	if got := call("good", "whsec-live"); got != http.StatusOK {
		t.Errorf("correctly signed request: status %d, want 200", got)
	}
	if got := call("good", "wrong-secret"); got != http.StatusUnauthorized {
		t.Errorf("wrongly signed request: status %d, want 401", got)
	}
	if got := call("good", ""); got != http.StatusUnauthorized {
		t.Errorf("unsigned request: status %d, want 401", got)
	}
	if got := call("empty", "anything"); got != http.StatusUnauthorized {
		t.Errorf("webhook with no secret set: status %d, want 401", got)
	}
	if got := call("broken", "anything"); got != http.StatusInternalServerError {
		t.Errorf("webhook whose secret cannot be decrypted: status %d, want 500", got)
	}
	if got := call("outbound", "whsec-live"); got != http.StatusNotFound {
		t.Errorf("outgoing webhook on the inbound route: status %d, want 404", got)
	}
}
