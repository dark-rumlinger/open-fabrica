package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestHMACSHA256HexVectors pins the exact wire output with known-answer vectors.
// The expected strings were computed with the code path this helper replaced
// (hmac.New(sha256.New, key); mac.Write(message); hex.EncodeToString(mac.Sum(nil)))
// in triggers, webhooks, and server/public, so any change here would break
// signature verification for external webhook consumers. The first two vectors
// are also the standard RFC 4231 / classic HMAC-SHA256 test values, so they are
// independently checkable.
func TestHMACSHA256HexVectors(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		message string
		want    string
	}{
		{
			// RFC 4231 test case 2.
			name:    "rfc4231_case2",
			key:     "Jefe",
			message: "what do ya want for nothing?",
			want:    "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
		},
		{
			name:    "quick_brown_fox",
			key:     "key",
			message: "The quick brown fox jumps over the lazy dog",
			want:    "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8",
		},
		{
			name:    "empty_key_and_message",
			key:     "",
			message: "",
			want:    "b613679a0814d9ec772f95d778c35fc5ff1697c493715653c6c712144292c5ad",
		},
		{
			// A webhook-shaped payload, as signed on the wire.
			name:    "webhook_payload",
			key:     "super-secret-signing-key",
			message: `{"event":"order.paid","project_id":1,"payload":{"amount":1999}}`,
			want:    "4298dfcba90af380ab96b7c1d74be807565d465002bfcec068f13e89fd960bb0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HMACSHA256Hex([]byte(c.key), []byte(c.message)); got != c.want {
				t.Errorf("HMACSHA256Hex(%q, %q) = %s, want %s", c.key, c.message, got, c.want)
			}
		})
	}
}

// TestHMACSHA256HexMatchesOldInlinePath proves byte-for-byte equivalence with
// the inline signing block the helper consolidated: the removed computeHMAC
// copies and the webhooks dispatcher both did exactly this sequence.
func TestHMACSHA256HexMatchesOldInlinePath(t *testing.T) {
	old := func(key, message []byte) string {
		mac := hmac.New(sha256.New, key)
		mac.Write(message)
		return hex.EncodeToString(mac.Sum(nil))
	}
	cases := []struct{ key, message string }{
		{"secret", `{"hello":"world"}`},
		{"", "message with empty key"},
		{"key-only", ""},
		{"binary\x00key", "binary\x00message\xff"},
	}
	for _, c := range cases {
		if got, want := HMACSHA256Hex([]byte(c.key), []byte(c.message)), old([]byte(c.key), []byte(c.message)); got != want {
			t.Errorf("HMACSHA256Hex(%q, %q) = %s, old path = %s", c.key, c.message, got, want)
		}
	}
}
