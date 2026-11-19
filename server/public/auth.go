package public

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openfabrica/events"
	"openfabrica/internal/tokenrevoke"
	"openfabrica/security"
	"openfabrica/server/middleware"
	"openfabrica/store"
)

// authEndpointConfig holds the configuration for auto-generated auth routes.
type authEndpointConfig struct {
	ID             int
	TableName      string
	Path           string
	UsernameColumn string
	PasswordColumn string
	PublicColumns  []string
	SignupColumns  []string
	JWTExpiryHours int
	DefaultRole    string
	RoleColumn     string
	// RateLimit is the requests/min-per-IP cap applied to register/login on this
	// endpoint, set via manage_auth create/update. Enforced in handleAuthRequest.
	RateLimit int
	// TenantSignupMode is "none" (no tenancy on this auth endpoint) or one of
	// "invite_only", "signup_creates_tenant", "signup_joins_default" once
	// manage_tenancy enable has been called. Drives register flow.
	TenantSignupMode string
}

// loadAuthEndpoint loads an auth endpoint configuration from the per-project database.
func (h *Handler) loadAuthEndpoint(projectDB *sql.DB, path string) (*authEndpointConfig, error) {
	row, err := store.LoadPublicAuthEndpoint(projectDB, path)
	if err != nil {
		return nil, err
	}
	ae := authEndpointConfig{
		ID:               row.ID,
		TableName:        row.TableName,
		Path:             row.Path,
		UsernameColumn:   row.UsernameColumn,
		PasswordColumn:   row.PasswordColumn,
		JWTExpiryHours:   row.JWTExpiryHours,
		DefaultRole:      row.DefaultRole,
		RoleColumn:       row.RoleColumn,
		RateLimit:        row.RateLimit,
		TenantSignupMode: row.TenantSignupMode,
	}
	if ae.RateLimit <= 0 {
		ae.RateLimit = 60
	}
	_ = json.Unmarshal([]byte(row.PublicColsJSON), &ae.PublicColumns) // malformed config degrades to no public columns
	_ = json.Unmarshal([]byte(row.SignupColsJSON), &ae.SignupColumns) // malformed config degrades to the default filter
	if ae.JWTExpiryHours == 0 {
		ae.JWTExpiryHours = 24
	}
	if ae.DefaultRole == "" {
		ae.DefaultRole = "user"
	}
	if ae.RoleColumn == "" {
		ae.RoleColumn = "role"
	}

	// Validate the table and column names to prevent SQL injection via stored config.
	// TableName feeds every auth query (SELECT/INSERT/PRAGMA), so it must clear the
	// same identifier grammar the columns do; a malformed table_name fails closed here
	// instead of reaching a query string.
	if err := security.ValidateTableName(ae.TableName); err != nil {
		return nil, fmt.Errorf("invalid table_name in auth config: %w", err)
	}
	if err := security.ValidateColumnName(ae.UsernameColumn); err != nil {
		return nil, fmt.Errorf("invalid username_column in auth config: %w", err)
	}
	if err := security.ValidateColumnName(ae.PasswordColumn); err != nil {
		return nil, fmt.Errorf("invalid password_column in auth config: %w", err)
	}
	if err := security.ValidateColumnNames(ae.PublicColumns); err != nil {
		return nil, fmt.Errorf("invalid public_columns in auth config: %w", err)
	}
	if err := security.ValidateColumnName(ae.RoleColumn); err != nil {
		return nil, fmt.Errorf("invalid role_column in auth config: %w", err)
	}
	if err := security.ValidateColumnNames(ae.SignupColumns); err != nil {
		return nil, fmt.Errorf("invalid signup_columns in auth config: %w", err)
	}

	return &ae, nil
}

// handleAuthRequest handles /api/{path}/register, /api/{path}/login, /api/{path}/me.
// Returns true if the request was handled as an auth endpoint.
// readDB is the multi-connection read pool; writeDB is the single-connection write pool.
func (h *Handler) handleAuthRequest(w http.ResponseWriter, r *http.Request, projectID int, readDB *sql.DB, writeDB *sql.DB, fullPath string) bool {
	// Check if the path matches an auth endpoint pattern: {authPath}/{action}
	parts := strings.SplitN(fullPath, "/", 2)
	if len(parts) != 2 {
		return false
	}

	authPath := parts[0]
	action := parts[1]

	// Try to load auth endpoint config, uses read pool (no writes needed).
	ae, err := h.loadAuthEndpoint(readDB, authPath)
	if err != nil {
		return false // Not an auth endpoint, let DynamicAPI handle it.
	}

	// Auth requests carry small credential/JSON bodies (login, register, magic,
	// totp, tenant switch). Cap the body so a large POST to a public auth
	// endpoint cannot exhaust memory. Uploads never reach this path.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB

	physTable := ae.TableName

	switch action {
	case "register":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return true
		}
		if !h.authLimiter.Allow(middleware.RateLimitIdentity(r)) {
			writeError(w, http.StatusTooManyRequests, "too many auth requests, slow down")
			return true
		}
		if !h.checkEndpointRateLimit(fmt.Sprintf("auth:%d:%s:%s", projectID, authPath, middleware.RateLimitIdentity(r)), ae.RateLimit) {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return true
		}
		h.authRegister(w, r, projectID, readDB, writeDB, physTable, ae)
		return true
	case "login":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return true
		}
		if !h.authLimiter.Allow(middleware.RateLimitIdentity(r)) {
			writeError(w, http.StatusTooManyRequests, "too many auth requests, slow down")
			return true
		}
		if !h.checkEndpointRateLimit(fmt.Sprintf("auth:%d:%s:%s", projectID, authPath, middleware.RateLimitIdentity(r)), ae.RateLimit) {
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return true
		}
		h.authLogin(w, r, projectID, readDB, writeDB, physTable, ae)
		return true
	case "me":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "GET required")
			return true
		}
		h.authMe(w, r, projectID, readDB, physTable, ae)
		return true
	case "refresh":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST required")
			return true
		}
		h.authRefresh(w, r, projectID, readDB, physTable, ae)
		return true
	}

	// OAuth routes: oauth/{provider} or oauth/{provider}/callback
	if strings.HasPrefix(action, "oauth/") {
		oauthParts := strings.SplitN(action[6:], "/", 2) // strip "oauth/" prefix
		provider := oauthParts[0]
		if len(oauthParts) == 2 && oauthParts[1] == "callback" {
			h.oauthCallback(w, r, projectID, readDB, writeDB, physTable, ae, provider)
		} else {
			h.oauthAuthorize(w, r, readDB, ae, provider)
		}
		return true
	}

	// TOTP routes: totp/setup, totp/verify, totp/disable, totp/login
	if strings.HasPrefix(action, "totp/") {
		sub := action[5:]
		h.handleTOTP(w, r, projectID, readDB, writeDB, physTable, ae, sub)
		return true
	}

	// Password reset routes: password/forgot, password/reset
	if strings.HasPrefix(action, "password/") {
		h.handlePassword(w, r, projectID, readDB, writeDB, physTable, ae, action[9:])
		return true
	}

	// Magic-link routes: magic/request, magic/verify
	if strings.HasPrefix(action, "magic/") {
		sub := action[6:]
		h.handleMagic(w, r, projectID, readDB, writeDB, physTable, ae, sub)
		return true
	}

	// Tenancy sub-routes: tenants (list), tenant/switch.
	if h.handleTenantSubroute(w, r, projectID, readDB, physTable, ae, action) {
		return true
	}

	return false
}

// privilegedSignupColumns are column names a registrant must never set: they grant
// rights or standing, so they stay at their table defaults.
var privilegedSignupColumns = tokenrevoke.AccountStandingColumns

// filterSignupBody limits which request fields reach the INSERT. With
// signup_columns set only the username and those columns pass; otherwise
// privilege-like columns are dropped. Password and role are assigned afterwards.
func filterSignupBody(body map[string]any, ae *authEndpointConfig) {
	if len(ae.SignupColumns) > 0 {
		allowed := map[string]bool{ae.UsernameColumn: true}
		for _, c := range ae.SignupColumns {
			allowed[c] = true
		}
		for col := range body {
			if !allowed[col] {
				delete(body, col)
			}
		}
		return
	}
	for col := range body {
		if col != ae.UsernameColumn && privilegedSignupColumns[strings.ToLower(col)] {
			delete(body, col)
		}
	}
}

// registrationFailure maps an INSERT error to a client status and a message that
// carries no SQL detail.
func registrationFailure(err error) (int, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return http.StatusConflict, "an account with these details already exists"
	case strings.Contains(msg, "NOT NULL constraint failed"):
		if i := strings.LastIndex(msg, "."); i >= 0 && i+1 < len(msg) {
			return http.StatusBadRequest, "missing required field: " + msg[i+1:]
		}
		return http.StatusBadRequest, "missing required field"
	}
	return http.StatusBadRequest, "registration failed"
}

// authRegister handles POST /api/{path}/register.
// readDB is used for SELECT queries; writeDB is used only for the final INSERT.
// Password hashing happens before touching writeDB to avoid blocking the write pool.
func (h *Handler) authRegister(w http.ResponseWriter, r *http.Request, projectID int, readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate required fields.
	username, ok := body[ae.UsernameColumn].(string)
	if !ok || username == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s is required", ae.UsernameColumn))
		return
	}
	password, ok := body["password"].(string)
	if !ok || password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return
	}
	if len(password) > maxPasswordBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("password must be at most %d bytes", maxPasswordBytes))
		return
	}
	delete(body, "password")
	filterSignupBody(body, ae)

	// Check if user already exists (read pool, no write lock needed).
	if _, err := store.FindAuthUserIDByUsername(readDB, physTable, ae.UsernameColumn, username); err == nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("user with this %s already exists", ae.UsernameColumn))
		return
	}

	// An invite-only endpoint must not create the account first and fail on the
	// invite after: that leaves an orphan user and lets anyone squat usernames.
	if ae.TenantSignupMode == "invite_only" && !h.inviteUsable(readDB, r.URL.Query().Get("invite"), username) {
		writeError(w, http.StatusForbidden, "invalid or expired invite")
		return
	}

	// Hash the password BEFORE touching the write pool, this is CPU-intensive
	// and must not block the single write connection.
	hashedPassword, err := security.HashPassword(password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}
	body[ae.PasswordColumn] = hashedPassword

	// Force default role (prevent self-assignment of admin, etc.).
	body[ae.RoleColumn] = ae.DefaultRole

	// Remove id if provided.
	delete(body, "id")

	// Load actual table columns to filter out unknown request fields (e.g.
	// "confirm_password", "terms_accepted"), which would otherwise fail the INSERT.
	// Uses read pool; PRAGMA is a read operation.
	if info, colErr := store.GetTableColumnInfo(readDB, physTable); colErr == nil {
		validCols := make(map[string]bool, len(info))
		for _, c := range info {
			validCols[c.Name] = true
		}
		if len(validCols) == 0 {
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("registration failed: table %q does not exist", ae.TableName))
			return
		}

		// Verify the auth config columns actually exist in the table.
		if !validCols[ae.UsernameColumn] || !validCols[ae.PasswordColumn] {
			writeError(w, http.StatusInternalServerError,
				fmt.Sprintf("auth config mismatch: table %q is missing column %q or %q",
					ae.TableName, ae.UsernameColumn, ae.PasswordColumn))
			return
		}

		// Filter unknown fields, only inside the validCols guard.
		for col := range body {
			if !validCols[col] {
				delete(body, col)
			}
		}
	}

	// Insert the row, validate column names from request body.
	columns := make([]string, 0, len(body))
	values := make([]any, 0, len(body))
	for col, val := range body {
		if err := security.ValidateColumnName(col); err != nil {
			writeError(w, http.StatusBadRequest, "invalid field name: "+col)
			return
		}
		columns = append(columns, security.QuoteIdent(col))
		values = append(values, val)
	}

	if len(columns) == 0 {
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("no valid columns for registration, table=%q, username_col=%q, password_col=%q, role_col=%q",
				ae.TableName, ae.UsernameColumn, ae.PasswordColumn, ae.RoleColumn))
		return
	}

	// Only the INSERT uses the write pool, all prior work used the read pool.
	userID, err := store.InsertAuthUser(writeDB, physTable, columns, values)
	if err != nil {
		status, msg := registrationFailure(err)
		slog.Warn("registration insert failed", "table", physTable, "error", err)
		writeError(w, status, msg)
		return
	}

	// Tenancy: if the auth endpoint has a signup mode, resolve/insert the
	// membership before minting the token so the new JWT carries tenant_id.
	tenantID, tenantRole, errMsg, status := h.applyRegistrationTenancy(r, writeDB, projectID, int(userID), username, ae)
	if status != 0 {
		// The account must not outlive a failed signup step (a consumed invite, a
		// missing default tenant): remove it so the username is not squatted.
		if delErr := store.DeleteAuthUserByID(writeDB, physTable, userID); delErr != nil {
			slog.Warn("could not remove user after failed signup", "table", physTable, "user_id", userID, "error", delErr)
		}
		writeError(w, status, errMsg)
		return
	}

	// Generate JWT token (with tenant claims if tenancy applied).
	var token string
	if tenantID > 0 {
		token, err = h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: projectID, UserID: int(userID), TenantID: tenantID, Username: username, Role: ae.DefaultRole, TenantRole: tenantRole, Expiry: authTokenExpiry(ae)})
	} else {
		token, err = h.generateUserToken(int(userID), username, ae.DefaultRole, projectID, ae)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	// Non-sensitive fields from the registration body ride the event for action templates.
	fields := map[string]any{}
	for col, val := range body {
		if col != ae.PasswordColumn && col != "id" {
			fields[col] = val
		}
	}
	h.publishAuthRegister(projectID, ae, int(userID), authMethodPassword, fields)

	writeJSON(w, http.StatusCreated, map[string]any{
		"success": true,
		"user_id": userID,
		"token":   token,
	})
}

// authLogin handles POST /api/{path}/login.
// Login is entirely read-only (SELECT + bcrypt compare), no write pool needed.
func (h *Handler) authLogin(w http.ResponseWriter, r *http.Request, projectID int, readDB, writeDB *sql.DB, physTable string, ae *authEndpointConfig) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	username, ok := body[ae.UsernameColumn].(string)
	if !ok || username == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s is required", ae.UsernameColumn))
		return
	}
	password, ok := body["password"].(string)
	if !ok || password == "" {
		writeError(w, http.StatusBadRequest, "password is required")
		return
	}

	// Per-account lockout (keyed project:endpoint:username), independent of IP.
	lockKey := fmt.Sprintf("%d:%s:%s", projectID, ae.TableName, strings.ToLower(strings.TrimSpace(username)))
	if locked, retryAfter := h.authLockout.Locked(lockKey); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}

	// Find the user by username (include role column). Read pool, no writes.
	userID, hashedPassword, roleStr, err := store.FindAuthUserCredentials(readDB, physTable, ae.UsernameColumn, ae.PasswordColumn, ae.RoleColumn, username)
	if err != nil {
		// Equalize timing with the wrong-password path so a missing user is
		// not distinguishable by response latency (username enumeration).
		security.DummyPasswordCompare()
		h.authLockout.Fail(lockKey)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	role := sql.NullString{String: roleStr, Valid: roleStr != ""}

	// Verify password, CPU-intensive but doesn't touch the database.
	if !security.CheckPassword(password, hashedPassword) {
		h.authLockout.Fail(lockKey)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	// The failure record is cleared only once the whole login succeeds (below),
	// so an attacker who has the password cannot reset it by guessing codes.

	// Second factor: if this user has a verified TOTP secret, a valid code is
	// mandatory before a token is issued, else enrolled 2FA is bypassable with the
	// password alone. The client resends login with totp_code after seeing needs_totp.
	//
	// Only while TOTP is enabled on this endpoint: manage_2fa disable removes that row,
	// and a disabled factor must stop challenging, otherwise enrolled users stay
	// gated by a feature whose /totp routes now answer 404.
	//
	// With enforced=true, a user who has NOT enrolled yet gets no session either: login
	// answers needs_totp_setup plus a short-lived enrollment token that is good for
	// exactly /totp/setup and /totp/verify, and the successful verify is what signs
	// them in. That is what makes "enforced" mean every account has a second factor.
	var totpSecretEnc string
	var totpVerified bool
	var totpEnabled, totpEnforced bool
	if _, enforced, err := store.GetTOTPEndpointConfig(readDB, ae.Path); err == nil {
		totpEnabled = true
		totpEnforced = enforced
	}
	totpEnrolled := false
	if totpEnabled {
		var err error
		totpSecretEnc, totpVerified, err = store.GetUserTOTP(readDB, userID)
		totpEnrolled = err == nil && totpVerified
	}
	if totpEnabled && !totpEnrolled && totpEnforced {
		h.authLockout.Reset(lockKey) // the password was right; nothing here lets a code be guessed
		h.startTOTPEnrollment(w, writeDB, projectID, userID, username, ae)
		return
	}
	if totpEnrolled {
		code, _ := body["totp_code"].(string)
		code = strings.TrimSpace(code)
		if code == "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"success":    false,
				"needs_totp": true,
				"message":    "two-factor code required",
			})
			return
		}
		if h.deps.Encryptor == nil {
			writeError(w, http.StatusInternalServerError, "encryption not available")
			return
		}
		secret, decErr := h.deps.Encryptor.Decrypt(totpSecretEnc)
		if decErr != nil {
			writeError(w, http.StatusInternalServerError, "totp decrypt failed")
			return
		}
		if ok, wait := h.verifyTOTPFor(projectID, userID, secret, code); !ok {
			if wait > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
				return
			}
			h.authLockout.Fail(lockKey)
			writeError(w, http.StatusUnauthorized, "invalid two-factor code")
			return
		}
	}
	h.authLockout.Reset(lockKey)

	userRole := role.String
	if userRole == "" {
		userRole = ae.DefaultRole
	}
	h.completeLogin(w, r, loginSubject{
		projectID: projectID,
		readDB:    readDB,
		ae:        ae,
		userID:    userID,
		username:  username,
		userRole:  userRole,
		method:    authMethodPassword,
	}, nil)
}

// Sign-in methods, carried as `method` on auth.register / auth.login so a trigger can
// tell them apart.
const (
	authMethodPassword  = "password"
	authMethodOAuth     = "oauth"
	authMethodMagicLink = "magic_link"
)

// publishAuthRegister raises auth.register for a user row that was just created, by
// any sign-in method. fields are the non-secret columns the event exposes to templates.
func (h *Handler) publishAuthRegister(projectID int, ae *authEndpointConfig, userID int, method string, fields map[string]any) {
	if h.deps.Bus == nil {
		return
	}
	payload := make(map[string]any, len(fields)+3)
	for k, v := range fields {
		payload[k] = v
	}
	payload["user_id"] = userID
	payload["table"] = ae.TableName
	payload["method"] = method
	h.deps.Bus.Publish(events.NewEvent(events.EventAuthRegister, projectID, payload))
}

// publishAuthLogin raises auth.login once a session token has been issued, by any
// sign-in method.
func (h *Handler) publishAuthLogin(projectID int, ae *authEndpointConfig, userID int, username, role, method string) {
	if h.deps.Bus == nil {
		return
	}
	h.deps.Bus.Publish(events.NewEvent(events.EventAuthLogin, projectID, map[string]any{
		"user_id":         userID,
		"table":           ae.TableName,
		ae.UsernameColumn: username,
		"role":            role,
		"method":          method,
	}))
}

// completeLogin is the end of a login once every factor has passed: it issues the
// session token (folding in the caller's active tenant when tenancy is on), answers the
// tenant-selection cases, publishes auth.login, and writes the response. extra is merged
// into the success body, so the TOTP enrollment verify can report what it just did.
// loginSubject is the authenticated principal and endpoint context a completed login
// needs: who logged in (userID/username/userRole), where (projectID, the auth endpoint
// config, and the read-pool handle that resolves tenancy), and how (method).
type loginSubject struct {
	projectID int
	readDB    *sql.DB
	ae        *authEndpointConfig
	userID    int
	username  string
	userRole  string
	method    string
}

func (h *Handler) completeLogin(w http.ResponseWriter, r *http.Request, s loginSubject, extra map[string]any) {
	// Generate JWT token. When tenancy is enabled on the endpoint, fold the caller's
	// active tenant into the claims; if they have multiple memberships, return the
	// list so the client can re-post login with ?tenant=<slug>.
	preferredSlug := r.URL.Query().Get("tenant")
	token, memberships, _, err := h.generateUserTokenWithTenant(s.readDB, s.projectID, s.userID, s.username, s.userRole, preferredSlug, s.ae)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	if token == "" {
		// Two cases: no membership (signup needed), or multiple and no preference.
		body := map[string]any{}
		if len(memberships) == 0 {
			body["success"] = false
			body["needs_tenant"] = true
			body["signup_mode"] = s.ae.TenantSignupMode
			body["message"] = "no active tenant for this user"
		} else {
			body["success"] = false
			body["needs_tenant_select"] = true
			body["tenants"] = memberships
		}
		for k, v := range extra {
			body[k] = v
		}
		writeJSON(w, http.StatusOK, body)
		return
	}

	h.publishAuthLogin(s.projectID, s.ae, s.userID, s.username, s.userRole, s.method)

	body := map[string]any{
		"success": true,
		"user_id": s.userID,
		"token":   token,
	}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, http.StatusOK, body)
}

// authMe handles GET /api/{path}/me.
// Pure read operation, uses read pool only.
func (h *Handler) authMe(w http.ResponseWriter, r *http.Request, projectID int, readDB *sql.DB, physTable string, ae *authEndpointConfig) {
	// Extract and validate JWT token, must be scoped to this project.
	claims, err := h.extractUserClaims(r)
	if err != nil || claims.ProjectID != projectID {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Fetch user data (excluding password columns). The platform-owned requireRole
	// helper (js/auth.js) gates on user.role from this row, so the role column is
	// always surfaced even when the model omitted it from public_columns; without it
	// /me returns no role field and every role-gated page bounces the logged-in user
	// home. The password column is never selected.
	colList := []string{"id", ae.UsernameColumn}
	if len(ae.PublicColumns) > 0 {
		colList = append([]string{}, ae.PublicColumns...)
	}
	colList = ensureColumn(colList, ae.RoleColumn)
	quotedCols := make([]string, len(colList))
	for i, c := range colList {
		quotedCols[i] = security.QuoteIdent(c)
	}

	userRow, err := store.LoadAuthUserMe(readDB, physTable, quotedCols, claims.UserID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "query error")
		return
	}

	// The platform auth helper gates on `user.role`, whatever the table calls its role
	// column. Surface the value under `role` too so a custom role_column ("user_role")
	// still passes requireRole instead of bouncing every user.
	if ae.RoleColumn != "" && ae.RoleColumn != "role" {
		if _, taken := userRow["role"]; !taken {
			userRow["role"] = userRow[ae.RoleColumn]
		}
	}

	writeJSON(w, http.StatusOK, userRow)
}

// authRefresh handles POST /api/{path}/refresh.
// Accepts a valid JWT and returns a new token with a fresh expiry.
// The user must still exist in the database; the token must be scoped to this project.
func (h *Handler) authRefresh(w http.ResponseWriter, r *http.Request, projectID int, readDB *sql.DB, physTable string, ae *authEndpointConfig) {
	claims, err := h.extractUserClaims(r)
	if err != nil || claims.ProjectID != projectID {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Verify the user still exists in the database.
	if !store.AuthUserExists(readDB, physTable, claims.UserID) {
		writeError(w, http.StatusUnauthorized, "user no longer exists")
		return
	}

	// The new token is built from current state, not from the old claims: the role
	// is re-read, and a tenanted session keeps its tenant only while the user is
	// still an active member of it.
	token, status, err := h.mintSession(readDB, projectID, claims.UserID, claims.TenantID, claims.Username, physTable, ae)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"user_id": claims.UserID,
		"token":   token,
	})
}

// ensureColumn appends col to cols if not already present, so /me can guarantee
// the role column is selected without duplicating it. An empty col is a no-op.
func ensureColumn(cols []string, col string) []string {
	if col == "" {
		return cols
	}
	for _, c := range cols {
		if c == col {
			return cols
		}
	}
	return append(cols, col)
}

// authTokenExpiry converts an auth endpoint's configured JWT lifetime into a
// duration. Returns 0 when unset so the JWT manager default applies.
func authTokenExpiry(ae *authEndpointConfig) time.Duration {
	if ae != nil && ae.JWTExpiryHours > 0 {
		return time.Duration(ae.JWTExpiryHours) * time.Hour
	}
	return 0
}

// currentUserRole reads the user's role from their row, falling back to the
// endpoint's default when the column is empty or unreadable.
func currentUserRole(readDB *sql.DB, physTable string, ae *authEndpointConfig, userID int) string {
	return store.ReadAuthUserRole(readDB, physTable, ae.RoleColumn, userID, ae.DefaultRole)
}

// mintSession re-issues a session for a user who already holds one (refresh,
// tenant switch). Role and tenant role come from the database, never from the
// old token. tenantID 0 mints a tenantless token; otherwise the user must still
// be an active member of that tenant. On failure it returns the HTTP status to
// answer with.
func (h *Handler) mintSession(readDB *sql.DB, projectID, userID, tenantID int, username, physTable string, ae *authEndpointConfig) (string, int, error) {
	if h.deps.JWTManager == nil {
		return "", http.StatusInternalServerError, fmt.Errorf("failed to generate token")
	}
	req := security.TokenRequest{
		ProjectID: projectID, UserID: userID, Username: username,
		Role: currentUserRole(readDB, physTable, ae, userID), Expiry: authTokenExpiry(ae),
	}
	if tenantID > 0 {
		memberships, err := h.loadActiveMemberships(readDB, userID)
		if err != nil {
			return "", http.StatusInternalServerError, fmt.Errorf("loading memberships")
		}
		found := false
		for _, m := range memberships {
			if m.TenantID == tenantID {
				req.TenantID, req.TenantRole, found = m.TenantID, m.Role, true
				break
			}
		}
		if !found {
			return "", http.StatusForbidden, fmt.Errorf("not a member of that tenant")
		}
	}
	token, err := h.deps.JWTManager.Generate(req)
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("failed to generate token")
	}
	return token, 0, nil
}

// generateUserToken creates a JWT token for a project user, scoped to the
// project, honoring the auth endpoint's configured token lifetime.
func (h *Handler) generateUserToken(userID int, username, role string, projectID int, ae *authEndpointConfig) (string, error) {
	if h.deps.JWTManager == nil {
		return "", fmt.Errorf("JWT manager not configured")
	}
	return h.deps.JWTManager.Generate(security.TokenRequest{ProjectID: projectID, UserID: userID, Username: username, Role: role, Expiry: authTokenExpiry(ae)})
}

// extractUserClaims extracts and validates JWT claims from the Authorization header.
func (h *Handler) extractUserClaims(r *http.Request) (*security.Claims, error) {
	if h.deps.JWTManager == nil {
		return nil, fmt.Errorf("JWT manager not configured")
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, fmt.Errorf("no authorization header")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == authHeader {
		return nil, fmt.Errorf("invalid authorization format")
	}

	return h.validateUserClaims(token)
}

// errTokenRevoked is returned for a signed, unexpired token whose user has had
// their sessions ended since it was issued.
var errTokenRevoked = fmt.Errorf("session ended")

// validateUserClaims validates a project user's token: the signature and expiry,
// and that the user's sessions have not been ended since it was issued (password
// reset, deleted or banned user, changed role; see internal/tokenrevoke). Every
// place in the public server that accepts a user token goes through here, so there
// is one answer to "is this token still good".
func (h *Handler) validateUserClaims(token string) (*security.Claims, error) {
	claims, err := h.deps.JWTManager.Validate(token)
	if err != nil {
		return nil, err
	}
	if claims.ProjectID > 0 && h.deps.ProjectDBManager != nil && h.deps.ProjectDBManager.Exists(claims.ProjectID) {
		pdb, err := h.deps.ProjectDBManager.Open(claims.ProjectID)
		if err == nil {
			var issued *time.Time
			if claims.IssuedAt != nil {
				issued = &claims.IssuedAt.Time
			}
			if tokenrevoke.Revoked(pdb.Reader(), claims.ProjectID, claims.UserID, issued) {
				return nil, errTokenRevoked
			}
		}
	}
	return claims, nil
}

// revokeUserSessions ends every session of one of the project's users. Failing to
// record it is logged, not fatal to the change that caused it, but it is the
// difference between a password reset that ends old sessions and one that does not,
// so it is logged loudly.
func (h *Handler) revokeUserSessions(writeDB *sql.DB, projectID, userID int, why string) {
	if err := tokenrevoke.Revoke(writeDB, projectID, userID); err != nil {
		slog.Error("could not end the user's existing sessions", "project_id", projectID, "user_id", userID, "reason", why, "error", err)
	}
}

// projectUserClaims validates a user token for this project and returns its
// claims. ok is false when the token is missing, invalid, revoked, or was
// issued for another project. Every endpoint kind that accepts a caller token
// goes through here.
func (h *Handler) projectUserClaims(projectID int, token string) (claims *security.Claims, ok bool) {
	if token == "" || h.deps.JWTManager == nil {
		return nil, false
	}
	claims, err := h.validateUserClaims(token)
	if err != nil || claims.ProjectID != projectID {
		return nil, false
	}
	return claims, true
}
