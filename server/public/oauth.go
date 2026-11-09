package public

import (
	"crypto/hmac"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"openfabrica/internal/netguard"
	"openfabrica/security"
)

// --- OAuth (Social Login) ---

type oauthProviderConfig struct {
	ID               int
	Name             string
	DisplayName      string
	ClientID         string
	ClientSecretName string
	AuthorizeURL     string
	TokenURL         string
	UserinfoURL      string
	Scopes           string
	UsernameField    string
	AuthEndpointPath string
}

func loadOAuthProvider(projectDB *sql.DB, providerName, authPath string) (*oauthProviderConfig, error) {
	var op oauthProviderConfig
	err := projectDB.QueryRow(
		"SELECT id, name, display_name, client_id, client_secret_name, authorize_url, token_url, userinfo_url, scopes, username_field, auth_endpoint_path FROM of_oauth_providers WHERE name = ? AND auth_endpoint_path = ? AND is_enabled = 1",
		providerName, authPath,
	).Scan(&op.ID, &op.Name, &op.DisplayName, &op.ClientID, &op.ClientSecretName, &op.AuthorizeURL, &op.TokenURL, &op.UserinfoURL, &op.Scopes, &op.UsernameField, &op.AuthEndpointPath)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// oauthAuthorize handles GET /api/{authPath}/oauth/{provider}, redirects to the OAuth provider.
func (h *Handler) oauthAuthorize(w http.ResponseWriter, r *http.Request, readDB *sql.DB, ae *authEndpointConfig, provider string) {
	op, err := loadOAuthProvider(readDB, provider, ae.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "OAuth provider not configured")
		return
	}

	// Generate HMAC-signed state for CSRF protection.
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate state")
		return
	}
	stateRandom := hex.EncodeToString(randomBytes)
	stateMAC := hex.EncodeToString(h.deps.JWTManager.HMAC(randomBytes))
	state := stateRandom + "." + stateMAC

	// Store state in a short-lived cookie (SameSite=Lax for cross-site redirect back).
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/api/" + ae.Path + "/oauth/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})

	redirectURI := oauthRedirectURI(r, ae.Path, provider)

	// Build authorization URL.
	authURL := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&state=%s",
		op.AuthorizeURL,
		url.QueryEscape(op.ClientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape(op.Scopes),
		url.QueryEscape(state),
	)

	http.Redirect(w, r, authURL, http.StatusFound)
}

// oauthCallback handles GET /api/{authPath}/oauth/{provider}/callback: exchanges code for token.
func (h *Handler) oauthCallback(w http.ResponseWriter, r *http.Request, projectID int, readDB *sql.DB, writeDB *sql.DB, physTable string, ae *authEndpointConfig, provider string) {
	// Validate state parameter.
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing OAuth state cookie")
		return
	}
	queryState := r.URL.Query().Get("state")
	if queryState == "" || queryState != stateCookie.Value {
		writeError(w, http.StatusBadRequest, "invalid OAuth state")
		return
	}
	// Verify HMAC on state.
	parts := strings.SplitN(queryState, ".", 2)
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "malformed OAuth state")
		return
	}
	randomBytes, err := hex.DecodeString(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid OAuth state encoding")
		return
	}
	expectedMAC := hex.EncodeToString(h.deps.JWTManager.HMAC(randomBytes))
	if !hmac.Equal([]byte(parts[1]), []byte(expectedMAC)) {
		writeError(w, http.StatusBadRequest, "OAuth state verification failed")
		return
	}
	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    "",
		Path:     "/api/" + ae.Path + "/oauth/",
		MaxAge:   -1,
		HttpOnly: true,
	})

	// Check for error from provider.
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		writeError(w, http.StatusBadRequest, "OAuth error: "+errParam)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "missing authorization code")
		return
	}

	// Load OAuth provider config (read pool).
	op, err := loadOAuthProvider(readDB, provider, ae.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "OAuth provider not configured")
		return
	}

	// Decrypt client secret (read pool).
	var encryptedSecret string
	err = readDB.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = ?", op.ClientSecretName).Scan(&encryptedSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "OAuth client secret not found")
		return
	}
	clientSecret, err := h.deps.Encryptor.Decrypt(encryptedSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to decrypt OAuth secret")
		return
	}

	redirectURI := oauthRedirectURI(r, ae.Path, provider)

	// Exchange authorization code for access token.
	tokenResp, err := exchangeOAuthCode(op, code, redirectURI, clientSecret)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to exchange OAuth code: "+err.Error())
		return
	}

	// Fetch user info from provider.
	userInfo, err := fetchOAuthUserInfo(op, tokenResp.AccessToken)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch user info: "+err.Error())
		return
	}

	// Extract username from the configured field.
	username, ok := userInfo[op.UsernameField].(string)
	if !ok || username == "" {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("user info missing %s field", op.UsernameField))
		return
	}

	// Find or create user in the project's user table.
	var userID int
	var role sql.NullString
	err = readDB.QueryRow(
		fmt.Sprintf("SELECT id, %s FROM %s WHERE %s = ?", security.QuoteIdent(ae.RoleColumn), security.QuoteIdent(physTable), security.QuoteIdent(ae.UsernameColumn)),
		username,
	).Scan(&userID, &role)
	if err == sql.ErrNoRows {
		// Create new user with an unusable password and default role.
		// Hash before write pool, CPU-intensive.
		unusableHash, _ := security.HashPassword(hex.EncodeToString(randomBytes) + "!oauth-no-password-login")
		result, err := writeDB.Exec(
			fmt.Sprintf("INSERT INTO %s (%s, %s, %s) VALUES (?, ?, ?)", security.QuoteIdent(physTable), security.QuoteIdent(ae.UsernameColumn), security.QuoteIdent(ae.PasswordColumn), security.QuoteIdent(ae.RoleColumn)),
			username, unusableHash, ae.DefaultRole,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create user")
			return
		}
		id, _ := result.LastInsertId()
		userID = int(id)
		role.String = ae.DefaultRole
		h.publishAuthRegister(projectID, ae, userID, authMethodOAuth, map[string]any{ae.UsernameColumn: username})
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	userRole := role.String
	if userRole == "" {
		userRole = ae.DefaultRole
	}

	// Second factor: OAuth proves identity but not the TOTP an enrolled user set up.
	purpose, stepToken, err := h.totpStepUp(readDB, writeDB, ae, userID, username, authMethodOAuth)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "two-factor check failed")
		return
	}
	if purpose != "" {
		writeTOTPStepUp(w, r, ae, purpose, stepToken, true)
		return
	}

	// Generate JWT.
	jwtToken, err := h.generateUserToken(userID, username, userRole, projectID, ae)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	h.publishAuthLogin(projectID, ae, userID, username, userRole, authMethodOAuth)

	// Redirect to frontend with the token in the URL fragment, not a query param.
	// Fragments are never sent to the server, preventing token leakage in server
	// logs, Referer headers, and browser history. Same key as the magic-link redirect.
	http.Redirect(w, r, "/#auth_token="+url.QueryEscape(jwtToken), http.StatusFound)
}

type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// requestIsHTTPS reports whether the public-facing request arrived over TLS. TLS is
// terminated upstream (tlsproxy), so r.TLS is nil and the proxy forwards
// X-Forwarded-Proto: https; honoring it keeps the OAuth redirect_uri and cookie
// flags correct behind the proxy.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// oauthRedirectURI builds the OAuth callback URI, using the project's configured
// domain to prevent open-redirect attacks via Host header manipulation.
// Falls back to r.Host only when no domain is configured (local development).
func oauthRedirectURI(r *http.Request, authPath, provider string) string {
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if project := getProject(r); project != nil && project.Domain != nil && *project.Domain != "" {
		host = *project.Domain
	}
	return fmt.Sprintf("%s://%s/api/%s/oauth/%s/callback", scheme, host, authPath, provider)
}

// oauthHTTPClient returns an SSRF-hardened HTTP client for an OAuth provider's
// token/userinfo endpoints. The provider's own configured host may resolve to a
// private IP (so a self-hosted provider on a LAN keeps working), while redirects or
// DNS-rebinding to any other internal address are blocked. An unparseable URL yields
// an empty allow-host, so nothing is exempted and a private target fails closed.
func oauthHTTPClient(rawURL string) *http.Client {
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Hostname()
	}
	client := netguard.SSRFSafeClientAllowingHost(host)
	client.Timeout = 10 * time.Second
	return client
}

func exchangeOAuthCode(op *oauthProviderConfig, code, redirectURI, clientSecret string) (*oauthTokenResponse, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {op.ClientID},
		"client_secret": {clientSecret},
	}

	client := oauthHTTPClient(op.TokenURL)
	req, err := http.NewRequest(http.MethodPost, op.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (status %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp oauthTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		// Some providers (GitHub) return form-encoded responses.
		vals, _ := url.ParseQuery(string(body))
		tokenResp.AccessToken = vals.Get("access_token")
		tokenResp.TokenType = vals.Get("token_type")
	}
	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("no access_token in response")
	}

	return &tokenResp, nil
}

func fetchOAuthUserInfo(op *oauthProviderConfig, accessToken string) (map[string]any, error) {
	client := oauthHTTPClient(op.UserinfoURL)
	req, err := http.NewRequest(http.MethodGet, op.UserinfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo request failed (status %d)", resp.StatusCode)
	}

	var info map[string]any
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("invalid userinfo JSON: %w", err)
	}

	return info, nil
}
