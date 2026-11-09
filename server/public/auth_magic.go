package public

// Magic-link passwordless login. Two endpoints:
//   POST /api/<auth>/magic/request {email} -> sends an email with a one-time link
//   GET  /api/<auth>/magic/verify?token=... -> redeems the token, issues a JWT
//                                              (sets the JSON {token, user_id})
// Tokens live in of_magic_tokens with a TTL, single-use.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"openfabrica/security"
	"openfabrica/server/middleware"
)

// magicTokenHash is what of_magic_tokens stores: the link carries the raw token,
// the database only ever holds its hash, so a copy of the table cannot sign anyone in.
func magicTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) handleMagic(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig, sub string) {

	var ttlMinutes int
	var emailTemplate, redirectURL string
	if err := readDB.QueryRow(
		"SELECT email_template, redirect_url, ttl_minutes FROM of_magic_link_endpoints WHERE path = ?",
		ae.Path,
	).Scan(&emailTemplate, &redirectURL, &ttlMinutes); err != nil {
		writeError(w, http.StatusNotFound, "magic-link not enabled for this auth endpoint")
		return
	}

	switch sub {
	case "request":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}
		// Each request enqueues an email send: throttle harder than ordinary
		// auth to keep abusers from burning through the project's email quota.
		if !h.magicRequestLimiter.Allow(middleware.RateLimitIdentity(r)) {
			writeError(w, http.StatusTooManyRequests, "too many magic-link requests, slow down")
			return
		}
		h.magicRequest(w, r, projectID, readDB, writeDB, physTable, ae, ttlMinutes, emailTemplate)
	case "verify":
		// Throttle redemption attempts per IP: tokens are 256-bit so brute force
		// is infeasible, but this keeps a leaked-token replay or grinding loop
		// from hammering the endpoint.
		if !h.magicVerifyLimiter.Allow(middleware.RateLimitIdentity(r)) {
			writeError(w, http.StatusTooManyRequests, "too many verification attempts, slow down")
			return
		}
		h.magicVerify(w, r, projectID, readDB, writeDB, physTable, ae, redirectURL)
	default:
		writeError(w, http.StatusNotFound, "unknown magic sub-route")
	}
}

func (h *Handler) magicRequest(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig,
	ttlMinutes int, emailTemplate string) {

	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}
	email := strings.TrimSpace(body.Email)

	// Per-address throttle on top of the per-IP one: many IPs can otherwise flood
	// one inbox. The reply is the same generic success, so it reveals nothing.
	if h.magicEmailLimiter != nil && !h.magicEmailLimiter.Allow("email:"+strings.ToLower(email)) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "If that address is allowed, a link is on its way."})
		return
	}
	// Spent and long-expired tokens have no use; drop them as requests arrive.
	_, _ = writeDB.Exec("DELETE FROM of_magic_tokens WHERE created_at < datetime('now', '-1 day')")

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "rand: "+err.Error())
		return
	}
	token := hex.EncodeToString(tokenBytes)
	expires := time.Now().Add(time.Duration(ttlMinutes) * time.Minute)

	// Look up existing user by the auth's username column. Match nil means
	// the verify step creates a new account on redemption.
	var existingID sql.NullInt64
	if validTableName.MatchString(physTable) && validTableName.MatchString(ae.UsernameColumn) {
		_ = readDB.QueryRow(
			fmt.Sprintf("SELECT id FROM %s WHERE %s = ?", security.QuoteIdent(physTable), security.QuoteIdent(ae.UsernameColumn)), email,
		).Scan(&existingID)
	}

	if _, err := writeDB.Exec(`
		INSERT INTO of_magic_tokens (token, auth_endpoint_path, email, user_id, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		magicTokenHash(token), ae.Path, email, existingID, expires,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "save token: "+err.Error())
		return
	}

	// The link's origin comes from the project's domain, never from the Host header:
	// a forged Host would otherwise mail the victim a link to the attacker's server.
	base, ok := publicBaseURL(r)
	if !ok {
		slog.Warn("magic link not sent: request host is not a local address and the project has no domain", "project_id", projectID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "If that address is allowed, a link is on its way."})
		return
	}
	verifyURL := fmt.Sprintf("%s/api/%s/magic/verify?token=%s", base, ae.Path, token)

	// Best-effort email send. The email_template lookup happens server-side
	// in the of_email_templates table; if missing we send a minimal default.
	subject := "Your sign-in link"
	htmlBody := fmt.Sprintf(`<p>Click to sign in: <a href="%s">%s</a></p><p>This link expires in %d minutes.</p>`, verifyURL, verifyURL, ttlMinutes)
	if emailTemplate != "" {
		var s, hb string
		_ = readDB.QueryRow("SELECT subject, body_html FROM of_email_templates WHERE name = ?", emailTemplate).Scan(&s, &hb) // missing template falls back to the default email
		if s != "" {
			subject = s
		}
		if hb != "" {
			htmlBody = strings.ReplaceAll(hb, "{{magic_url}}", verifyURL)
		}
	}
	// Enqueue the email via of_jobs so the worker delivers it through the
	// project's of_email_config. This avoids importing action_runner from the
	// public HTTP layer.
	jobPayload := fmt.Sprintf(`{"to":%q,"subject":%q,"body_html":%q}`, email, subject, htmlBody)
	if _, err := writeDB.Exec(`INSERT INTO of_jobs (type, payload, status, max_attempts) VALUES ('send_email', ?, 'pending', 3)`, jobPayload); err != nil {
		writeError(w, http.StatusInternalServerError, "could not queue sign-in email")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// Always 200, never reveal whether the email is registered.
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "If that address is allowed, a link is on its way."})
}

func (h *Handler) magicVerify(w http.ResponseWriter, r *http.Request, projectID int,
	readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig, redirectURL string) {

	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}

	var email string
	var userIDN sql.NullInt64
	var expires time.Time
	var used bool
	if err := readDB.QueryRow(
		`SELECT email, user_id, expires_at, used
		 FROM of_magic_tokens WHERE token = ? AND auth_endpoint_path = ? AND purpose = 'login'`,
		magicTokenHash(token), ae.Path,
	).Scan(&email, &userIDN, &expires, &used); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if used || time.Now().After(expires) {
		writeError(w, http.StatusUnauthorized, "token expired or already used")
		return
	}

	// Mark used immediately so this token is single-use even if the redirect
	// is replayed. This must succeed before we mint a JWT: if the write fails
	// and we issued a token anyway, the link would stay redeemable until TTL.
	// The WHERE used = 0 makes the claim atomic: of two concurrent redemptions only
	// one sees a row change, so a link cannot mint two sessions or two accounts.
	res, err := writeDB.Exec("UPDATE of_magic_tokens SET used = 1 WHERE token = ? AND used = 0", magicTokenHash(token))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not redeem token")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusUnauthorized, "token expired or already used")
		return
	}

	userID := int(userIDN.Int64)
	role := ae.DefaultRole
	if userID == 0 {
		// First-time login -> create the user with a random unguessable password.
		randomPwd := make([]byte, 24)
		rand.Read(randomPwd)
		hash, err := security.HashPassword(hex.EncodeToString(randomPwd))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "hash: "+err.Error())
			return
		}
		if !validTableName.MatchString(physTable) ||
			!validTableName.MatchString(ae.UsernameColumn) ||
			!validTableName.MatchString(ae.PasswordColumn) ||
			!validTableName.MatchString(ae.RoleColumn) {
			writeError(w, http.StatusInternalServerError, "invalid auth config")
			return
		}
		stmt := fmt.Sprintf("INSERT INTO %s (%s, %s, %s) VALUES (?, ?, ?)",
			security.QuoteIdent(physTable), security.QuoteIdent(ae.UsernameColumn), security.QuoteIdent(ae.PasswordColumn), security.QuoteIdent(ae.RoleColumn))
		res, err := writeDB.Exec(stmt, email, hash, ae.DefaultRole)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create user: "+err.Error())
			return
		}
		newID, err := res.LastInsertId()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create user: could not read new user id")
			return
		}
		userID = int(newID)
		h.publishAuthRegister(projectID, ae, userID, authMethodMagicLink, map[string]any{ae.UsernameColumn: email})
	} else if validTableName.MatchString(physTable) && validTableName.MatchString(ae.RoleColumn) {
		_ = readDB.QueryRow(fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", security.QuoteIdent(ae.RoleColumn), security.QuoteIdent(physTable)), userID).Scan(&role) // missing role column keeps the default role
	}

	// Second factor: the email link proves the inbox, not the TOTP an enrolled user set up.
	browser := strings.Contains(r.Header.Get("Accept"), "text/html") || redirectURL != ""
	purpose, stepToken, err := h.totpStepUp(readDB, writeDB, ae, userID, email, authMethodMagicLink)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "two-factor check failed")
		return
	}
	if purpose != "" {
		writeTOTPStepUp(w, r, ae, purpose, stepToken, browser)
		return
	}

	jwtToken, err := h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: projectID, UserID: userID, Username: email, Role: role, Expiry: authTokenExpiry(ae)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "jwt: "+err.Error())
		return
	}
	h.publishAuthLogin(projectID, ae, userID, email, role, authMethodMagicLink)

	// Decide response shape based on Accept header.
	if browser {
		dest := redirectURL
		if dest == "" {
			dest = "/"
		}
		// Hand the JWT to the SPA via the URL fragment. Fragments are not sent
		// in the Referer header and never reach the server, so the token does
		// not leak in logs or upstream proxies. The SPA reads location.hash,
		// stores the token in localStorage('auth_token'), then clears the hash.
		// See tools/manage_auth.go for the client-side pattern.
		if u, err := url.Parse(dest); err == nil {
			u.Fragment = "auth_token=" + jwtToken
			dest = u.String()
		} else {
			dest = dest + "#auth_token=" + jwtToken
		}
		http.Redirect(w, r, dest, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "user_id": userID, "token": jwtToken,
	})
}
