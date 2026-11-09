package public

import (
	"fmt"
	"sync"
	"time"

	"openfabrica/security"
)

// maxTOTPReplayEntries bounds the replay table; one entry per account that used
// a code, cleared wholesale if it ever fills.
const maxTOTPReplayEntries = 100_000

var (
	totpReplayMu   sync.Mutex
	totpReplayLast = map[string]int64{}
)

// acceptTOTPStep records step as used for key and reports whether it was fresh. A
// code is valid for about 90 seconds, so without this a code observed once could
// be replayed inside that window.
func acceptTOTPStep(key string, step int64) bool {
	totpReplayMu.Lock()
	defer totpReplayMu.Unlock()
	if last, ok := totpReplayLast[key]; ok && step <= last {
		return false
	}
	if len(totpReplayLast) >= maxTOTPReplayEntries {
		clear(totpReplayLast)
	}
	totpReplayLast[key] = step
	return true
}

// verifyTOTPFor checks a second-factor code for one account. Failures count
// against a per-account lockout (an attacker who knows the password could
// otherwise guess the six digits at the per-IP rate), and a code that was already
// accepted is refused. retryAfter is non-zero when the account is locked.
func (h *Handler) verifyTOTPFor(projectID, userID int, secret, code string) (ok bool, retryAfter time.Duration) {
	key := fmt.Sprintf("totp:%d:%d", projectID, userID)
	if h.authLockout != nil {
		if locked, wait := h.authLockout.Locked(key); locked {
			return false, wait
		}
	}
	step, valid := security.VerifyTOTPStep(secret, code, time.Now().UTC())
	if valid && !acceptTOTPStep(key, step) {
		valid = false
	}
	if !valid {
		if h.authLockout != nil {
			h.authLockout.Fail(key)
		}
		return false, 0
	}
	if h.authLockout != nil {
		h.authLockout.Reset(key)
	}
	return true, 0
}
