package messaging

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"math/big"
	"strings"
	"testing"
)

// Generated keys must use the stored wire format (65-byte uncompressed
// public point, 32-byte private scalar), and the private scalar alone must
// round-trip back to the exact same public key bytes, signVAPID rebuilds
// the keypair from just the scalar, so any drift here breaks delivery.
func TestGenerateVAPIDKeypair_RoundTrip(t *testing.T) {
	pubB64, privB64, err := GenerateVAPIDKeypair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pub, err := decodeB64URL(pubB64)
	if err != nil || len(pub) != 65 || pub[0] != 0x04 {
		t.Fatalf("public key must be a 65-byte uncompressed point, got %d bytes (err %v)", len(pub), err)
	}
	priv, err := decodeB64URL(privB64)
	if err != nil || len(priv) != 32 {
		t.Fatalf("private key must be a 32-byte scalar, got %d bytes (err %v)", len(priv), err)
	}

	key, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		t.Fatalf("stored scalar must parse as a P-256 private key: %v", err)
	}
	if !bytes.Equal(key.PublicKey().Bytes(), pub) {
		t.Fatalf("public key derived from stored scalar differs from stored public key")
	}
}

// A subscription endpoint that resolves to a loopback/private/link-local
// address must be rejected before any network dial. The endpoint is supplied by
// a browser PushSubscription and is therefore attacker-influenceable, so this is
// the SSRF guard for the web-push egress path (regression test for that fix).
func TestSendWebPush_RejectsPrivateEndpoint(t *testing.T) {
	// Literal IPs keep this hermetic (no DNS resolution needed).
	endpoints := []string{
		"http://127.0.0.1:5734/push",              // admin control plane
		"http://169.254.169.254/latest/meta-data", // cloud metadata
		"http://10.0.0.5/push",                    // RFC-1918
	}
	for _, endpoint := range endpoints {
		gone, err := SendWebPush(nil, endpoint, "", "", []byte("{}"), "", "", "ops@example.net")
		if err == nil {
			t.Fatalf("endpoint %q: expected SSRF rejection, got nil error", endpoint)
		}
		if gone {
			t.Fatalf("endpoint %q: a rejected endpoint must not be reported as gone (would prune a valid sub)", endpoint)
		}
		if !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("endpoint %q: error should indicate the endpoint was rejected, got: %v", endpoint, err)
		}
	}
}

// A JWT signed with the stored private key must verify against the stored
// public key (the push service does exactly this check).
func TestSignVAPID_SignatureVerifies(t *testing.T) {
	pubB64, privB64, err := GenerateVAPIDKeypair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	jwt, pubHdr, err := signVAPID("https://push.example.net/send/abc", "ops@example.net", pubB64, privB64)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if pubHdr != pubB64 {
		t.Fatalf("k= header must be the stored public key")
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 segments, got %d", len(parts))
	}
	sig, err := decodeB64URL(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("ES256 signature must be 64 raw bytes, got %d (err %v)", len(sig), err)
	}

	pubBytes, _ := decodeB64URL(pubB64)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubBytes)
	if err != nil {
		t.Fatalf("parse public key: %v", err)
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, hash[:], r, s) {
		t.Fatalf("VAPID JWT signature does not verify against the public key")
	}
}
