package security

// TOTP (RFC 6238) for project end-user two-factor auth: secret minting, code
// generation, and verification. One home for the algorithm, shared by the tool
// that enables 2FA on an auth endpoint and the public server that serves
// /totp/setup and verifies logins.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- RFC 6238 TOTP is defined over HMAC-SHA1; authenticator apps expect it
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// totpEncoding is the unpadded base32 authenticator apps expect.
var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh 20-byte secret, base32-encoded.
func NewTOTPSecret() string {
	buf := make([]byte, 20)
	_, _ = rand.Read(buf) // crypto/rand.Read never fails on supported platforms
	return totpEncoding.EncodeToString(buf)
}

// VerifyTOTPCode reports whether code is valid for secretB32 at now. It accepts
// the current, previous, and next 30s windows for clock drift, and compares in
// constant time so a timing side channel can't leak the expected code.
func VerifyTOTPCode(secretB32, code string, now time.Time) bool {
	secret, err := totpEncoding.DecodeString(strings.ToUpper(secretB32))
	if err != nil {
		return false
	}
	t := now.Unix() / 30
	for delta := int64(-1); delta <= 1; delta++ {
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, t+delta)), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// TOTPCode is the 6-digit HOTP value (RFC 4226) for secret at counter.
func TOTPCode(secret []byte, counter int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter)) // #nosec G115 -- counter is a Unix time step, never negative
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	val := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	return fmt.Sprintf("%06d", val%1_000_000)
}

// VerifyTOTPStep is VerifyTOTPCode that also returns the 30s time step the code
// matched, so a caller can refuse a second use of the same code.
func VerifyTOTPStep(secretB32, code string, now time.Time) (int64, bool) {
	secret, err := totpEncoding.DecodeString(strings.ToUpper(secretB32))
	if err != nil {
		return 0, false
	}
	t := now.Unix() / 30
	for delta := int64(-1); delta <= 1; delta++ {
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, t+delta)), []byte(code)) == 1 {
			return t + delta, true
		}
	}
	return 0, false
}
