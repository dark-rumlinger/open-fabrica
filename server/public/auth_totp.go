package public

// TOTP 2FA endpoint handlers. RFC 6238 (HOTP/TOTP) implementation, SHA-1,
// 30s step, 6 digits. No external dep.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"openfabrica/internal/qrcode"
	"openfabrica/security"
	"openfabrica/server/middleware"
)

func (h *Handler) handleTOTP(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig, sub string) {

	// Enforce that TOTP is enabled for this auth endpoint.
	var enforced bool
	var issuer string
	if err := readDB.QueryRow(
		"SELECT issuer, enforced FROM of_totp_endpoints WHERE path = ?", ae.Path,
	).Scan(&issuer, &enforced); err != nil {
		writeError(w, http.StatusNotFound, "TOTP not enabled for this auth endpoint")
		return
	}

	switch sub {
	case "setup":
		h.totpSetup(w, r, projectID, readDB, writeDB, physTable, ae, issuer)
	case "verify":
		// Verify is a guess-six-digits surface: rate-limit per IP to defeat
		// brute force while still allowing a couple of legitimate retries.
		if !h.totpVerifyLimiter.Allow(middleware.RateLimitIdentity(r)) {
			writeError(w, http.StatusTooManyRequests, "too many TOTP attempts, slow down")
			return
		}
		h.totpVerify(w, r, projectID, readDB, writeDB, physTable, ae)
	case "disable":
		h.totpDisable(w, r, projectID, readDB, writeDB, ae)
	default:
		writeError(w, http.StatusNotFound, "unknown totp sub-route")
	}
}

// totpSetup: authenticated user calls this, server generates a fresh secret
// (or returns existing un-verified one), responds with otpauth:// URI for
// the QR code. Secret is encrypted at rest in of_user_totp.
func (h *Handler) totpSetup(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig, issuer string) {

	userID, _ := h.requireUserOrEnrollment(w, r, projectID, readDB, ae)
	if userID == 0 {
		return
	}

	var secretEnc string
	var verified bool
	err := readDB.QueryRow(
		"SELECT secret_encrypted, verified FROM of_user_totp WHERE user_id = ?", userID,
	).Scan(&secretEnc, &verified)

	var secret string
	if err == sql.ErrNoRows || (err == nil && !verified) {
		// Issue a new secret if none exists or the prior one was never verified.
		secret = security.NewTOTPSecret()
		if h.deps.Encryptor == nil {
			writeError(w, http.StatusInternalServerError, "encryption not available")
			return
		}
		enc, encErr := h.deps.Encryptor.Encrypt(secret)
		if encErr != nil {
			writeError(w, http.StatusInternalServerError, "encrypt: "+encErr.Error())
			return
		}
		_, dbErr := writeDB.Exec(`
			INSERT INTO of_user_totp (user_id, auth_endpoint_path, secret_encrypted, verified)
			VALUES (?, ?, ?, 0)
			ON CONFLICT(user_id) DO UPDATE SET secret_encrypted = excluded.secret_encrypted, verified = 0`,
			userID, ae.Path, enc)
		if dbErr != nil {
			writeError(w, http.StatusInternalServerError, "save: "+dbErr.Error())
			return
		}
	} else if err == nil {
		// Already verified -> reject setup so attacker can't rotate secret silently.
		writeError(w, http.StatusConflict, "TOTP already configured for this account")
		return
	} else {
		writeError(w, http.StatusInternalServerError, "db: "+err.Error())
		return
	}

	if issuer == "" {
		issuer = "OpenFabrica"
	}
	username := h.lookupUsername(readDB, physTable, ae, userID)
	otpauth := fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=6&period=30",
		url.PathEscape(issuer), url.PathEscape(username), secret, url.QueryEscape(issuer))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"secret":      secret,
		"otpauth_url": otpauth,
		// QR rendered locally (embedded SVG data URI) so the TOTP secret never
		// leaves this machine. No external QR service is contacted.
		"qr_url": qrcode.DataURI(otpauth),
	})
}

// totpVerify: confirms a code against the user's stored secret. On the
// first successful verify, marks the row verified=1 (completes setup).
func (h *Handler) totpVerify(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig) {

	userID, stepUp := h.requireUserOrEnrollment(w, r, projectID, readDB, ae)
	if userID == 0 {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // bad JSON leaves an empty code, rejected below
	code := strings.TrimSpace(body.Code)
	if len(code) < 6 {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	var enc string
	var verified bool
	if err := readDB.QueryRow(
		"SELECT secret_encrypted, verified FROM of_user_totp WHERE user_id = ?", userID,
	).Scan(&enc, &verified); err != nil {
		writeError(w, http.StatusNotFound, "totp not set up")
		return
	}
	secret, err := h.deps.Encryptor.Decrypt(enc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "decrypt failed")
		return
	}
	if ok, wait := h.verifyTOTPFor(projectID, userID, secret, code); !ok {
		if wait > 0 {
			writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid code")
		return
	}
	if !verified {
		// The response claims setup is complete, so the write must have stuck;
		// otherwise 2FA would silently never be enforced at login.
		if _, err := writeDB.Exec("UPDATE of_user_totp SET verified = 1, verified_at = CURRENT_TIMESTAMP WHERE user_id = ?", userID); err != nil {
			writeError(w, http.StatusInternalServerError, "save: "+err.Error())
			return
		}
	}
	if stepUp != nil {
		// The holder of a step-up token has no session yet: a correct code is what
		// completes the sign-in that was waiting on the second factor (an enforced
		// enrollment, or an OAuth / magic-link sign-in of an enrolled user). The token is
		// single use, claimed atomically so two racing verifies cannot both sign in.
		res, err := writeDB.Exec("UPDATE of_magic_tokens SET used = 1 WHERE token = ? AND used = 0", stepUp.hash)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "save: "+err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeError(w, http.StatusUnauthorized, "this enrollment link has expired or was already used")
			return
		}
		username := h.lookupUsername(readDB, physTable, ae, userID)
		role := ae.DefaultRole
		var dbRole sql.NullString
		if validTableName.MatchString(physTable) && validTableName.MatchString(ae.RoleColumn) {
			_ = readDB.QueryRow(fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", security.QuoteIdent(ae.RoleColumn), security.QuoteIdent(physTable)), userID).Scan(&dbRole) // no role column keeps the default role
		}
		if dbRole.Valid && dbRole.String != "" {
			role = dbRole.String
		}
		extra := map[string]any{"verified": true}
		if stepUp.purpose == totpPurposeEnroll {
			extra["enrolled"] = true
		}
		h.completeLogin(w, r, loginSubject{
			projectID: projectID,
			readDB:    readDB,
			ae:        ae,
			userID:    userID,
			username:  username,
			userRole:  role,
			method:    stepUp.method,
		}, extra)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "verified": true})
}

// Step-up token purposes in of_magic_tokens, and how long each stays usable.
const (
	totpPurposeEnroll = "totp_enroll" // enroll then verify: an enforced endpoint, a user not enrolled yet
	totpPurposeLogin  = "totp_login"  // verify only: an enrolled user signing in without the password form
	totpEnrollTTL     = 15 * time.Minute
	totpLoginTTL      = 10 * time.Minute
)

// stepUpToken is a step-up token presented to /totp/setup or /totp/verify in place of
// a session: its stored hash, its purpose, and the sign-in method it completes.
type stepUpToken struct {
	hash, purpose, method string
}

// mintStepUpToken stores a single-use step-up token for userID and returns it. An
// earlier unused token of the same purpose for the account stops working.
func mintStepUpToken(writeDB *sql.DB, ae *authEndpointConfig, userID int, username, purpose, method string, ttl time.Duration) (string, error) {
	if writeDB == nil {
		return "", fmt.Errorf("no database")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	_, _ = writeDB.Exec("DELETE FROM of_magic_tokens WHERE purpose = ? AND user_id = ? AND auth_endpoint_path = ?", purpose, userID, ae.Path)
	if _, err := writeDB.Exec(
		`INSERT INTO of_magic_tokens (token, auth_endpoint_path, email, user_id, expires_at, purpose, method) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		magicTokenHash(token), ae.Path, username, userID, time.Now().Add(ttl), purpose, method,
	); err != nil {
		return "", err
	}
	return token, nil
}

// startTOTPEnrollment answers an enforced login from a user who has not enrolled. No
// session is issued; the reply carries a single-purpose token that /totp/setup and
// /totp/verify accept in place of a session.
func (h *Handler) startTOTPEnrollment(w http.ResponseWriter, writeDB *sql.DB, projectID, userID int,
	username string, ae *authEndpointConfig) {
	token, err := mintStepUpToken(writeDB, ae, userID, username, totpPurposeEnroll, authMethodPassword, totpEnrollTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start enrollment")
		return
	}
	writeEnrollmentReply(w, ae, token)
}

func writeEnrollmentReply(w http.ResponseWriter, ae *authEndpointConfig, token string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success":           false,
		"needs_totp_setup":  true,
		"enroll_token":      token,
		"expires_in":        int(totpEnrollTTL.Seconds()),
		"setup_url":         "/api/" + ae.Path + "/totp/setup",
		"verify_url":        "/api/" + ae.Path + "/totp/verify",
		"message":           "two-factor enrollment required: POST setup then verify with Authorization: Bearer <enroll_token>; the verify call signs you in",
		"enroll_token_note": "usable only for /totp/setup and /totp/verify",
	})
}

// totpStepUp is the second-factor gate for a sign-in that proved identity without the
// password form (OAuth, magic link), so it challenges exactly the users a password
// login challenges: an enrolled user must verify a code, and on an enforced endpoint
// an unenrolled user must enroll first. It returns the token purpose ("" when no step
// is needed) and the single-use token. A database error is returned, never treated as
// "no 2FA": a sign-in must not skip the factor because a read failed.
func (h *Handler) totpStepUp(readDB, writeDB *sql.DB, ae *authEndpointConfig, userID int, username, method string) (string, string, error) {
	var enforced bool
	if err := readDB.QueryRow("SELECT enforced FROM of_totp_endpoints WHERE path = ?", ae.Path).Scan(&enforced); err != nil {
		if err == sql.ErrNoRows {
			return "", "", nil
		}
		return "", "", err
	}
	var verified bool
	switch err := readDB.QueryRow("SELECT verified FROM of_user_totp WHERE user_id = ?", userID).Scan(&verified); {
	case err != nil && err != sql.ErrNoRows:
		return "", "", err
	case err == nil && verified:
		token, err := mintStepUpToken(writeDB, ae, userID, username, totpPurposeLogin, method, totpLoginTTL)
		return totpPurposeLogin, token, err
	case enforced:
		token, err := mintStepUpToken(writeDB, ae, userID, username, totpPurposeEnroll, method, totpEnrollTTL)
		return totpPurposeEnroll, token, err
	}
	return "", "", nil
}

// writeTOTPStepUp answers a sign-in that must pass the second factor first. A browser
// flow is redirected to /login with the token in the URL fragment (#totp_verify=... or
// #totp_setup=...), which the login page finishes through /totp/verify; a JSON caller
// gets the step-up shape instead. No session is issued either way.
func writeTOTPStepUp(w http.ResponseWriter, r *http.Request, ae *authEndpointConfig, purpose, token string, browser bool) {
	if browser {
		key := "totp_verify"
		if purpose == totpPurposeEnroll {
			key = "totp_setup"
		}
		http.Redirect(w, r, "/login#"+key+"="+token, http.StatusFound)
		return
	}
	if purpose == totpPurposeEnroll {
		writeEnrollmentReply(w, ae, token)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":           false,
		"needs_totp_verify": true,
		"totp_token":        token,
		"expires_in":        int(totpLoginTTL.Seconds()),
		"verify_url":        "/api/" + ae.Path + "/totp/verify",
		"message":           "two-factor code required: POST verify {code} with Authorization: Bearer <totp_token>; the verify call signs you in",
	})
}

// requireUserOrEnrollment authenticates /totp/setup and /totp/verify: a normal session
// token, or a step-up token a sign-in handed out (nil for a session). Writes 401 and
// returns 0 on failure.
func (h *Handler) requireUserOrEnrollment(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, ae *authEndpointConfig) (int, *stepUpToken) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return 0, nil
	}
	if h.deps.JWTManager != nil {
		if claims, err := h.validateUserClaims(token); err == nil && claims.ProjectID == projectID {
			return claims.UserID, nil
		}
	}
	hash := magicTokenHash(token)
	var userIDN sql.NullInt64
	var expires time.Time
	var used bool
	var purpose, method string
	if err := readDB.QueryRow(
		`SELECT user_id, expires_at, used, purpose, method FROM of_magic_tokens WHERE token = ? AND auth_endpoint_path = ? AND purpose IN (?, ?)`,
		hash, ae.Path, totpPurposeEnroll, totpPurposeLogin,
	).Scan(&userIDN, &expires, &used, &purpose, &method); err != nil || !userIDN.Valid || used || time.Now().After(expires) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return 0, nil
	}
	if method == "" {
		method = authMethodPassword
	}
	return int(userIDN.Int64), &stepUpToken{hash: hash, purpose: purpose, method: method}
}

func (h *Handler) totpDisable(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, ae *authEndpointConfig) {
	userID := h.requireUser(w, r, projectID)
	if userID == 0 {
		return
	}
	// Require a fresh TOTP code before disabling. Defence against stolen sessions.
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // bad JSON leaves an empty code, rejected below
	var enc string
	if err := readDB.QueryRow("SELECT secret_encrypted FROM of_user_totp WHERE user_id = ?", userID).Scan(&enc); err != nil {
		writeError(w, http.StatusNotFound, "totp not set up")
		return
	}
	secret, _ := h.deps.Encryptor.Decrypt(enc)
	if ok, wait := h.verifyTOTPFor(projectID, userID, secret, strings.TrimSpace(body.Code)); !ok {
		if wait > 0 {
			writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid code")
		return
	}
	// The response claims 2FA is off, so the delete must have stuck; otherwise
	// the user would keep being challenged for codes they meant to disable.
	if _, err := writeDB.Exec("DELETE FROM of_user_totp WHERE user_id = ?", userID); err != nil {
		writeError(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
}

// requireUser extracts the user_id from the bearer JWT. Writes 401 and
// returns 0 on failure.
func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request, projectID int) int {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if h.deps.JWTManager == nil || token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return 0
	}
	claims, err := h.validateUserClaims(token)
	if err != nil || claims.ProjectID != projectID {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return 0
	}
	return claims.UserID
}

func (h *Handler) lookupUsername(readDB *sql.DB, physTable string, ae *authEndpointConfig, userID int) string {
	if !validTableName.MatchString(physTable) || !validTableName.MatchString(ae.UsernameColumn) {
		return fmt.Sprintf("user-%d", userID)
	}
	var name string
	_ = readDB.QueryRow(fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", security.QuoteIdent(ae.UsernameColumn), security.QuoteIdent(physTable)), userID).Scan(&name) // falls back to user-<id> below
	if name == "" {
		name = fmt.Sprintf("user-%d", userID)
	}
	return name
}
