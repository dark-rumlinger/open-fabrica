package security

import "testing"

func TestProcessSecureValue_Hash(t *testing.T) {
	out, err := ProcessSecureValue("hash", "s3cret", nil)
	if err != nil {
		t.Fatalf("ProcessSecureValue hash: %v", err)
	}
	hash, ok := out.(string)
	if !ok {
		t.Fatalf("hash output is %T, want string", out)
	}
	if hash == "s3cret" {
		t.Error("hash output must not equal the plaintext")
	}
	if !CheckPassword("s3cret", hash) {
		t.Error("hashed value does not verify against the original plaintext")
	}
}

func TestProcessSecureValue_Encrypt(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	out, err := ProcessSecureValue("encrypt", "api-token-xyz", enc)
	if err != nil {
		t.Fatalf("ProcessSecureValue encrypt: %v", err)
	}
	ct, ok := out.(string)
	if !ok {
		t.Fatalf("encrypt output is %T, want string", out)
	}
	if ct == "api-token-xyz" {
		t.Error("encrypt output must not equal the plaintext")
	}
	got, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "api-token-xyz" {
		t.Errorf("decrypt mismatch: got %q want %q", got, "api-token-xyz")
	}
}

func TestProcessSecureValue_EncryptNonStringValue(t *testing.T) {
	enc := newTestEncryptor(t, encTestKey)
	// Non-string values are stringified via fmt before encryption.
	out, err := ProcessSecureValue("encrypt", 12345, enc)
	if err != nil {
		t.Fatalf("ProcessSecureValue encrypt int: %v", err)
	}
	ct := out.(string)
	got, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "12345" {
		t.Errorf("decrypt of stringified int: got %q want %q", got, "12345")
	}
}

func TestProcessSecureValue_EncryptWithoutEncryptorErrors(t *testing.T) {
	if _, err := ProcessSecureValue("encrypt", "value", nil); err == nil {
		t.Error("expected error when kind is 'encrypt' but no Encryptor is provided")
	}
}

func TestProcessSecureValue_DefaultPassesValueThrough(t *testing.T) {
	// An unrecognized kind returns the value untouched.
	out, err := ProcessSecureValue("plain", "unchanged", nil)
	if err != nil {
		t.Fatalf("ProcessSecureValue default: %v", err)
	}
	if out != "unchanged" {
		t.Errorf("default kind altered value: got %v want %q", out, "unchanged")
	}

	// Non-string values are passed through as-is (not stringified).
	out2, err := ProcessSecureValue("", 99, nil)
	if err != nil {
		t.Fatalf("ProcessSecureValue empty kind: %v", err)
	}
	if out2 != 99 {
		t.Errorf("default kind altered int value: got %v want 99", out2)
	}
}
