package public

import (
	"encoding/base32"
	"testing"
	"time"

	"openfabrica/security"
	"openfabrica/server/middleware"
)

func totpTestSecret() (string, []byte) {
	raw := []byte("12345678901234567890")
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), raw
}

// A second-factor code is single use: the same code, accepted once, must not
// pass again inside its ~90 second validity window.
func TestVerifyTOTPForRefusesReplay(t *testing.T) {
	h := &Handler{authLockout: middleware.NewAccountLockout(5, time.Minute, time.Hour)}
	secret, raw := totpTestSecret()
	code := security.TOTPCode(raw, time.Now().Unix()/30)

	if ok, _ := h.verifyTOTPFor(1, 100, secret, code); !ok {
		t.Fatal("a fresh valid code was refused")
	}
	if ok, _ := h.verifyTOTPFor(1, 100, secret, code); ok {
		t.Fatal("the same code was accepted twice")
	}
	// Another account is unaffected.
	if ok, _ := h.verifyTOTPFor(1, 101, secret, code); !ok {
		t.Error("replay state leaked across accounts")
	}
}

// Wrong codes lock the account, so six digits cannot be guessed at the per-IP
// rate by someone who already has the password.
func TestVerifyTOTPForLocksAfterRepeatedFailures(t *testing.T) {
	h := &Handler{authLockout: middleware.NewAccountLockout(3, time.Minute, time.Hour)}
	secret, raw := totpTestSecret()

	for i := 0; i < 3; i++ {
		if ok, _ := h.verifyTOTPFor(2, 200, secret, "000000"); ok {
			t.Fatal("a wrong code was accepted")
		}
	}
	good := security.TOTPCode(raw, time.Now().Unix()/30)
	ok, wait := h.verifyTOTPFor(2, 200, secret, good)
	if ok || wait <= 0 {
		t.Fatalf("locked account accepted a code or gave no wait: ok=%v wait=%v", ok, wait)
	}
}
