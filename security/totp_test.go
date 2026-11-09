package security

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B vectors (SHA1, secret "12345678901234567890"), truncated to
// the 6 digits the platform issues.
func TestTOTPCodeRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59:         "287082",
		1111111109: "081804",
		1234567890: "005924",
		2000000000: "279037",
	} {
		if got := TOTPCode(secret, unix/30); got != want {
			t.Errorf("TOTPCode at %d = %s, want %s", unix, got, want)
		}
	}
}

func TestVerifyTOTPCodeWindow(t *testing.T) {
	secretB32 := NewTOTPSecret()
	raw, err := totpEncoding.DecodeString(secretB32)
	if err != nil || len(raw) != 20 {
		t.Fatalf("NewTOTPSecret = %q (%d bytes, %v), want 20 base32 bytes", secretB32, len(raw), err)
	}
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / 30
	for _, delta := range []int64{-1, 0, 1} {
		if !VerifyTOTPCode(secretB32, TOTPCode(raw, step+delta), now) {
			t.Errorf("code for window %+d should verify (drift tolerance)", delta)
		}
	}
	if VerifyTOTPCode(secretB32, TOTPCode(raw, step+2), now) {
		t.Error("a code two windows out must not verify")
	}
	if VerifyTOTPCode("not base32!", "123456", now) {
		t.Error("an undecodable secret must not verify")
	}
}

func TestVerifyTOTPStepReportsTheMatchedWindow(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	raw, err := totpEncoding.DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / 30
	got, ok := VerifyTOTPStep(secret, TOTPCode(raw, step), now)
	if !ok || got != step {
		t.Errorf("current code: step=%d ok=%v, want %d true", got, ok, step)
	}
	got, ok = VerifyTOTPStep(secret, TOTPCode(raw, step-1), now)
	if !ok || got != step-1 {
		t.Errorf("previous-window code: step=%d ok=%v, want %d true", got, ok, step-1)
	}
	if _, ok := VerifyTOTPStep(secret, "000000", now); ok && TOTPCode(raw, step) != "000000" {
		t.Error("a wrong code verified")
	}
}
