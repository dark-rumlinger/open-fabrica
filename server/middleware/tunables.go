package middleware

import "time"

// Auth rate-limit and lockout tunables.
//
// These govern brute-force resistance on the login / magic-link / TOTP surfaces.
// They live here, in one place, so the whole policy can be reviewed and tuned
// without hunting for inline literals across the admin and public route trees.
// Rates are requests/second/IP (with the paired burst allowance); lockouts are
// per-account wrong-attempt thresholds with exponential backoff from Base to Max.
const (
	// Admin control-plane login (per IP), plus a per-account lockout. The admin
	// panel is loopback-bound by default, so the limits are modest.
	AdminLoginRatePerSec = 5
	AdminLoginBurst      = 10
	AdminLockoutAttempts = 5

	// Public project login (per IP) and its per-account lockout. Public sites are
	// always network-reachable, so the lockout tolerates a few more attempts
	// before engaging (legit users fat-finger passwords on phones).
	PublicLoginRatePerSec = 5
	PublicLoginBurst      = 10
	PublicLockoutAttempts = 10

	// Passwordless magic-link request (send email): deliberately slow to curb
	// email-bombing a victim's inbox.
	MagicRequestRatePerSec = 0.5
	MagicRequestBurst      = 3

	// Magic-link verification and TOTP verification (per IP): tight enough to
	// blunt code-guessing, loose enough for a legitimate retry.
	MagicVerifyRatePerSec = 2
	MagicVerifyBurst      = 10
	TOTPVerifyRatePerSec  = 2
	TOTPVerifyBurst       = 10
)

// Shared account-lockout backoff window: after the per-surface attempt threshold
// is crossed, delay grows exponentially from Base up to Max.
const (
	LockoutBaseDelay = 30 * time.Second
	LockoutMaxDelay  = 15 * time.Minute
)
