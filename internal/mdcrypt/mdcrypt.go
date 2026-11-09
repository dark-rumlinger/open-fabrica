// Package mdcrypt encrypts and decrypts the embedded knowledge files so the
// prompt text is not sitting in the binary as cleartext a `strings` dump or an
// embed.FS extractor can lift out. It is AES-256-GCM with a content-derived
// nonce, so encrypting the same bytes twice yields the same output and a
// regenerated tree only changes where the source changed (reproducible builds,
// no churn).
//
// Honest scope: with no server to hold the key, the key ships in this binary.
// Splitting it into XOR halves assembled at init keeps a 32-byte literal from
// being grep-able, but a determined analyst with a debugger can still recover
// it. This raises the bar against casual extraction; it is not a vault.
package mdcrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

// key is the real AES-256 key, rebuilt at init from two halves so it never
// appears as one contiguous 32-byte run in the binary. The two arrays are not
// secret on their own; their XOR is the key.
var key = assembleKey()

func assembleKey() []byte {
	a := [32]byte{
		0x3f, 0xa1, 0x08, 0xd4, 0x6c, 0x1b, 0x97, 0xe2,
		0x55, 0x80, 0x3a, 0xc9, 0x12, 0x7e, 0xbb, 0x04,
		0x9d, 0x6f, 0x21, 0xf8, 0x48, 0xa6, 0x13, 0x5c,
		0xe0, 0x2d, 0x88, 0x74, 0xcf, 0x19, 0x62, 0xab,
	}
	b := [32]byte{
		0x5c, 0x12, 0x9e, 0x37, 0x04, 0xdd, 0x6a, 0x21,
		0xb8, 0x4f, 0x93, 0x0c, 0x7d, 0xa5, 0x30, 0xe9,
		0x16, 0xc2, 0x58, 0x3b, 0x9f, 0x71, 0x44, 0x2a,
		0x87, 0xd0, 0x1e, 0x63, 0x05, 0xfa, 0x8c, 0x49,
	}
	k := make([]byte, 32)
	for i := range k {
		k[i] = a[i] ^ b[i]
	}
	return k
}

// nonceSize is GCM's standard 12 bytes; kept as a constant so Encrypt and
// Decrypt agree without constructing a cipher just to ask.
const nonceSize = 12

// Encrypt returns nonce||ciphertext for plaintext. The nonce is derived from
// name and plaintext via HMAC, which makes the output deterministic and keeps
// the (key, nonce) pair safe: a repeated nonce can only happen for identical
// (name, plaintext), never for two different payloads under the same key.
func Encrypt(name string, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM()
	if err != nil {
		return nil, err
	}
	nonce := nonceFor(name, plaintext)
	// Seal prepends nonce (first arg) so the blob carries what Decrypt needs.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt, reading the prepended nonce from the blob.
func Decrypt(blob []byte) ([]byte, error) {
	gcm, err := newGCM()
	if err != nil {
		return nil, err
	}
	if len(blob) < nonceSize {
		return nil, errors.New("mdcrypt: blob shorter than nonce")
	}
	nonce, ct := blob[:nonceSize], blob[nonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM() (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithNonceSize(block, nonceSize)
}

func nonceFor(name string, plaintext []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(name))
	mac.Write([]byte{0})
	mac.Write(plaintext)
	return mac.Sum(nil)[:nonceSize]
}
