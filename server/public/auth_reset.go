package public

// Password reset for an auth endpoint, over the same one-time-token table magic
// links use (purpose 'reset'):
//
//	POST /api/<auth>/password/forgot  {<username column>: "<email>"} -> emails a reset link
//	POST /api/<auth>/password/reset   {token, password}               -> sets the new password
//
// The link points at the project's own /reset page (which the builder creates and
// which posts the token back). Forgot always answers the same generic success, so
// it cannot be used to learn which addresses have an account.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"openfabrica/security"
	"openfabrica/server/middleware"
)

const (
	resetTokenTTL     = time.Hour
	maxPasswordBytes  = 72 // bcrypt silently ignores, then rejects, anything longer
	resetGenericReply = "If that address has an account, a reset link is on its way."
)

// publicBaseURL is the origin to put in an emailed link. The Host header is
// attacker-controlled, so it is trusted only for a local address; a project with a
// domain uses that domain. ok is false when no safe origin exists.
func publicBaseURL(r *http.Request) (string, bool) {
	if p := getProject(r); p != nil && p.Domain != nil && *p.Domain != "" {
		return "https://" + *p.Domain, true
	}
	if middleware.IsLoopbackHost(r.Host) {
		return "http://" + r.Host, true
	}
	return "", false
}

func (h *Handler) handlePassword(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig, sub string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	switch sub {
	case "forgot":
		if !h.authLimiter.Allow(middleware.RateLimitIdentity(r)) {
			writeError(w, http.StatusTooManyRequests, "too many requests, slow down")
			return
		}
		h.passwordForgot(w, r, projectID, readDB, writeDB, physTable, ae)
	case "reset":
		if !h.magicVerifyLimiter.Allow(middleware.RateLimitIdentity(r)) {
			writeError(w, http.StatusTooManyRequests, "too many attempts, slow down")
			return
		}
		h.passwordReset(w, r, projectID, readDB, writeDB, physTable, ae)
	default:
		writeError(w, http.StatusNotFound, "unknown password sub-route")
	}
}

func writeResetReply(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": resetGenericReply})
}

func (h *Handler) passwordForgot(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	username, _ := body[ae.UsernameColumn].(string)
	username = strings.TrimSpace(username)
	if username == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s is required", ae.UsernameColumn))
		return
	}
	// Per address, on top of per IP, so one inbox cannot be flooded from many IPs.
	if h.magicEmailLimiter != nil && !h.magicEmailLimiter.Allow("reset:"+strings.ToLower(username)) {
		writeResetReply(w)
		return
	}

	var userID int
	err := readDB.QueryRow(
		fmt.Sprintf("SELECT id FROM %s WHERE %s = ?", security.QuoteIdent(physTable), security.QuoteIdent(ae.UsernameColumn)),
		username,
	).Scan(&userID)
	if err != nil || !strings.Contains(username, "@") {
		// No such account, or the login name is not an address we can mail.
		writeResetReply(w)
		return
	}
	base, ok := publicBaseURL(r)
	if !ok {
		slog.Warn("password reset link not sent: request host is not a local address and the project has no domain", "project_id", projectID)
		writeResetReply(w)
		return
	}
	var configured int
	_ = readDB.QueryRow("SELECT COUNT(*) FROM of_email_config").Scan(&configured)
	if configured == 0 {
		slog.Warn("password reset requested but no email provider is configured", "project_id", projectID)
		writeResetReply(w)
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a reset link")
		return
	}
	token := hex.EncodeToString(raw)
	// Earlier unused reset links for this account stop working once a new one exists.
	_, _ = writeDB.Exec("DELETE FROM of_magic_tokens WHERE purpose = 'reset' AND user_id = ? AND auth_endpoint_path = ?", userID, ae.Path)
	if _, err := writeDB.Exec(
		`INSERT INTO of_magic_tokens (token, auth_endpoint_path, email, user_id, expires_at, purpose) VALUES (?, ?, ?, ?, ?, 'reset')`,
		magicTokenHash(token), ae.Path, username, userID, time.Now().Add(resetTokenTTL),
	); err != nil {
		writeError(w, http.StatusInternalServerError, "could not create a reset link")
		return
	}

	resetURL := base + "/reset?token=" + url.QueryEscape(token)
	subject := "Reset your password"
	htmlBody := fmt.Sprintf(`<p>Someone asked to reset the password for this account. To choose a new one, open <a href="%s">%s</a>.</p><p>The link works once and expires in 1 hour. If this was not you, ignore this email.</p>`, resetURL, resetURL)
	var s, hb string
	_ = readDB.QueryRow("SELECT subject, body_html FROM of_email_templates WHERE name = 'password_reset'").Scan(&s, &hb) // no template means the default email
	if s != "" {
		subject = s
	}
	if hb != "" {
		htmlBody = strings.ReplaceAll(hb, "{{reset_url}}", resetURL)
	}
	payload, _ := json.Marshal(map[string]string{"to": username, "subject": subject, "body_html": htmlBody})
	if _, err := writeDB.Exec(`INSERT INTO of_jobs (type, payload, status, max_attempts) VALUES ('send_email', ?, 'pending', 3)`, string(payload)); err != nil {
		slog.Warn("could not queue password reset email", "error", err)
	}
	writeResetReply(w)
}

func (h *Handler) passwordReset(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "token and password are required")
		return
	}
	if body.Password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return
	}
	if len(body.Password) > maxPasswordBytes {
		writeError(w, http.StatusBadRequest, "password must be at most "+strconv.Itoa(maxPasswordBytes)+" bytes")
		return
	}

	hash := magicTokenHash(body.Token)
	var email string
	var userIDN sql.NullInt64
	var expires time.Time
	var used bool
	if err := readDB.QueryRow(
		`SELECT email, user_id, expires_at, used FROM of_magic_tokens WHERE token = ? AND auth_endpoint_path = ? AND purpose = 'reset'`,
		hash, ae.Path,
	).Scan(&email, &userIDN, &expires, &used); err != nil || !userIDN.Valid {
		writeError(w, http.StatusUnauthorized, "this reset link is not valid")
		return
	}
	if used || time.Now().After(expires) {
		writeError(w, http.StatusUnauthorized, "this reset link has expired or was already used")
		return
	}

	newHash, err := security.HashPassword(body.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not set the password")
		return
	}
	// The claim is atomic: only one of two simultaneous submissions proceeds.
	res, err := writeDB.Exec("UPDATE of_magic_tokens SET used = 1 WHERE token = ? AND used = 0", hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not set the password")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusUnauthorized, "this reset link has expired or was already used")
		return
	}
	if _, err := writeDB.Exec(
		fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", security.QuoteIdent(physTable), security.QuoteIdent(ae.PasswordColumn)),
		newHash, userIDN.Int64,
	); err != nil {
		slog.Error("password reset could not update the account", "error", err)
		// Give the link back so the user can retry instead of being locked out.
		_, _ = writeDB.Exec("UPDATE of_magic_tokens SET used = 0 WHERE token = ?", hash)
		writeError(w, http.StatusInternalServerError, "could not set the password")
		return
	}
	// Sessions opened with the old password end, so whoever was in the account before
	// the reset (the reason it is usually done) is signed out everywhere.
	h.revokeUserSessions(writeDB, projectID, int(userIDN.Int64), "password reset")
	// A reset proves control of the address, so any lockout from failed guesses ends.
	h.authLockout.Reset(fmt.Sprintf("%d:%s:%s", projectID, ae.TableName, strings.ToLower(strings.TrimSpace(email))))
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Password updated. Sign in with the new password."})
}
