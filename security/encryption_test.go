package security

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A valid AES-256 key is 32 bytes hex-encoded (64 hex chars). Two distinct keys
// let us prove that decryption fails under the wrong key.
const (
	encTestKey  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	encTestKey2 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func newTestEncryptor(t *testing.T, hexKey string) *Encryptor {
	t.Helper()
	enc, err := NewEncryptor(hexKey)
	if err != nil {
		t.Fatalf("NewEncryptor(%q): %v", hexKey, err)
	}
	return enc
}

func TestNewEncryptor_KeyValidation(t *testing.T) {
	if _, err := NewEncryptor(encTestKey); err != nil {
		t.Errorf("valid 32-byte hex key rejected: %v", err)
	}
	// Not hex.
	if _, err := NewEncryptor("nothexnothexnothex"); err == nil {
		t.Error("expected error for non-hex key")
	}
	// Valid hex but wrong length (16 bytes, not 32).
	if _, err := NewEncryptor("0123456789abcdef0123456789abcdef"); err == nil {
		t.Error("expected error for 16-byte key")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	cases := []string{
		"hello world",
		"a secret value with symbols !@#$%^&*()",
		"unicode: naïve café 世界",
	}
	for _, plaintext := range cases {
		ct, err := enc.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plaintext, err)
		}
		got, err := enc.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != plaintext {
			t.Errorf("round-trip mismatch: got %q want %q", got, plaintext)
		}
	}
}

func TestEncrypt_EmptyStringRoundTrips(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	ct, err := enc.Encrypt("")
	if err != nil {
		t.Fatalf("Encrypt(\"\"): %v", err)
	}
	got, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "" {
		t.Errorf("empty round-trip: got %q want \"\"", got)
	}
}

func TestEncrypt_RandomNonceProducesDistinctCiphertexts(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	const plaintext = "same input every time"
	a, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt a: %v", err)
	}
	b, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt b: %v", err)
	}
	if a == b {
		t.Error("two encryptions of the same plaintext are identical; nonce is not random")
	}
	// Both must still decrypt back to the same plaintext.
	for _, ct := range []string{a, b} {
		got, err := enc.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != plaintext {
			t.Errorf("decrypt mismatch: got %q want %q", got, plaintext)
		}
	}
}

func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	ct, err := enc.Encrypt("authenticated payload")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Flip a bit in the last byte (part of the GCM tag/ciphertext) so
	// authentication must fail.
	raw[len(raw)-1] ^= 0x01
	tampered := base64.StdEncoding.EncodeToString(raw)
	if _, err := enc.Decrypt(tampered); err == nil {
		t.Error("expected Decrypt of tampered ciphertext to fail, got nil error")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	other := newTestEncryptor(t, encTestKey2)
	ct, err := enc.Encrypt("cross-key secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := other.Decrypt(ct); err == nil {
		t.Error("expected Decrypt with a different key to fail, got nil error")
	}
}

func TestDecrypt_MalformedInputFails(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	// Not valid base64.
	if _, err := enc.Decrypt("!!!not-base64!!!"); err == nil {
		t.Error("expected error for non-base64 input")
	}
	// Valid base64 but shorter than the nonce.
	short := base64.StdEncoding.EncodeToString([]byte{0x00, 0x01})
	if _, err := enc.Decrypt(short); err == nil {
		t.Error("expected error for ciphertext shorter than nonce")
	}
}

// SealOptional/OpenOptional keep "no secret" as an empty column, round-trip a real
// value through ciphertext that differs from it, and fail clearly without a key
// rather than store or return plaintext.
func TestSealOpenOptional(t *testing.T) {
	enc, err := NewEncryptor(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := enc.SealOptional(""); err != nil || got != "" {
		t.Errorf("SealOptional(\"\") = %q, %v; want empty", got, err)
	}
	if got, err := enc.OpenOptional(""); err != nil || got != "" {
		t.Errorf("OpenOptional(\"\") = %q, %v; want empty", got, err)
	}

	sealed, err := enc.SealOptional("whsec-123")
	if err != nil || sealed == "" || sealed == "whsec-123" || strings.Contains(sealed, "whsec") {
		t.Fatalf("SealOptional = %q, %v; want opaque ciphertext", sealed, err)
	}
	if got, err := enc.OpenOptional(sealed); err != nil || got != "whsec-123" {
		t.Errorf("OpenOptional round trip = %q, %v", got, err)
	}
	if _, err := enc.OpenOptional("not-ciphertext"); err == nil {
		t.Error("garbage was accepted as ciphertext")
	}

	var none *Encryptor
	if _, err := none.SealOptional("x"); err == nil {
		t.Error("sealed without a key")
	}
	if _, err := none.OpenOptional("x"); err == nil {
		t.Error("opened without a key")
	}
	if got, err := none.OpenOptional(""); err != nil || got != "" {
		t.Errorf("an empty value must not need a key: %q, %v", got, err)
	}
}
