package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// HMACSHA256Hex returns the lowercase hex encoding of HMAC-SHA256 over message
// keyed with key. It is the single signing primitive behind the webhook
// X-Webhook-Signature header: outgoing webhook delivery, the trigger_webhook
// action, and incoming-webhook verification all use it. The output must stay
// byte-identical across releases because external webhook consumers verify
// these signatures.
func HMACSHA256Hex(key, message []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return hex.EncodeToString(mac.Sum(nil))
}
