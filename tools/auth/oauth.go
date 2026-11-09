package auth

import (
	"fmt"
	"log/slog"
	"strings"

	"openfabrica/tools"
)

// OAuth provider handlers for manage_oauth. Schema lives in manage_oauth.go.

// OAuth provider actions

// oauthProviderConfig carries the options for registering an OAuth provider
// (manage_oauth create). It replaces a 10-argument all-string signature where a
// transposed pair (e.g. token_url vs userinfo_url) was invisible at the call site.
type oauthProviderConfig struct {
	ProviderName  string
	DisplayName   string
	ClientID      string
	ClientSecret  string
	AuthorizeURL  string
	TokenURL      string
	UserinfoURL   string
	Scopes        string
	UsernameField string
	AuthPath      string
}

func epCreateOAuth(ctx *tools.ToolContext, cfg oauthProviderConfig) (*tools.Result, error) {
	providerName := cfg.ProviderName
	displayName := cfg.DisplayName
	clientID := cfg.ClientID
	clientSecret := cfg.ClientSecret
	authorizeURL := cfg.AuthorizeURL
	tokenURL := cfg.TokenURL
	userinfoURL := cfg.UserinfoURL
	scopes := cfg.Scopes
	usernameField := cfg.UsernameField
	rawAuthPath := cfg.AuthPath

	authPath := tools.NormalizeEndpointPath(rawAuthPath)

	if providerName == "" || clientID == "" || clientSecret == "" || authorizeURL == "" || tokenURL == "" || userinfoURL == "" || authPath == "" {
		return tools.Fail("provider_name, client_id, client_secret, authorize_url, token_url, userinfo_url, and auth_path are required"), nil
	}
	if displayName == "" {
		displayName = providerName
	}

	if scopes == "" {
		scopes = "openid email profile"
	}
	if usernameField == "" {
		usernameField = "email"
	}

	// Skip if OAuth provider already exists (prevents duplicate creation during BUILD).
	var oauthExists int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_oauth_providers WHERE name = ?", providerName).Scan(&oauthExists); err == nil && oauthExists > 0 {
		return tools.Ok(map[string]any{
			"provider":        providerName,
			"already_existed": true,
			"message":         "OAuth provider " + providerName + " already exists, so nothing was created and NO option in this call was applied. Change its credentials or URLs with manage_oauth action=update.",
		}), nil
	}

	// Verify the auth endpoint exists.
	var authExists int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_auth_endpoints WHERE path = ?", authPath).Scan(&authExists)
	if authExists == 0 {
		return tools.Fail("auth endpoint '%s' does not exist, create it first with manage_auth action=create", authPath), nil
	}

	// Encrypt and store the client secret.
	secretName := "oauth_" + providerName + "_secret"
	if ctx.Encryptor == nil {
		return tools.Fail("encryption not available"), nil
	}
	encrypted, err := ctx.Encryptor.Encrypt(clientSecret)
	if err != nil {
		return tools.Fail("failed to encrypt client secret"), nil
	}
	if _, err := ctx.DB.Exec(
		`INSERT INTO of_secrets (name, value_encrypted) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
		secretName, encrypted,
	); err != nil {
		return tools.Fail("failed to store encrypted secret: %v", err), nil
	}

	_, err = ctx.DB.Exec(
		`INSERT INTO of_oauth_providers (name, display_name, client_id, client_secret_name, authorize_url, token_url, userinfo_url, scopes, username_field, auth_endpoint_path)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   display_name = excluded.display_name,
		   client_id = excluded.client_id,
		   client_secret_name = excluded.client_secret_name,
		   authorize_url = excluded.authorize_url,
		   token_url = excluded.token_url,
		   userinfo_url = excluded.userinfo_url,
		   scopes = excluded.scopes,
		   username_field = excluded.username_field,
		   auth_endpoint_path = excluded.auth_endpoint_path`,
		providerName, displayName, clientID, secretName, authorizeURL, tokenURL, userinfoURL, scopes, usernameField, authPath,
	)
	if err != nil {
		return nil, fmt.Errorf("creating OAuth provider: %w", err)
	}

	return tools.Ok(map[string]any{
		"provider": providerName,
		"routes": []string{
			fmt.Sprintf("GET /api/%s/oauth/%s (redirect to provider)", authPath, providerName),
			fmt.Sprintf("GET /api/%s/oauth/%s/callback (automatic)", authPath, providerName),
		},
		"display_name": displayName,
		"auth_path":    authPath,
	}), nil
}

func epListOAuth(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT id, name, display_name, client_id, authorize_url, token_url, userinfo_url, scopes, username_field, auth_endpoint_path, is_enabled, created_at FROM of_oauth_providers ORDER BY name",
	)
	if err != nil {
		return nil, fmt.Errorf("listing OAuth providers: %w", err)
	}
	defer rows.Close()

	var providers []map[string]any
	for rows.Next() {
		var id int
		var name, displayName, clientID, authorizeURL, tokenURL, userinfoURL, scopes, usernameField, authPath string
		var isEnabled bool
		var createdAt string
		if err := rows.Scan(&id, &name, &displayName, &clientID, &authorizeURL, &tokenURL, &userinfoURL, &scopes, &usernameField, &authPath, &isEnabled, &createdAt); err != nil {
			continue
		}
		providers = append(providers, map[string]any{
			"id":            id,
			"name":          name,
			"display_name":  displayName,
			"client_id":     clientID,
			"authorize_url": authorizeURL,
			"scopes":        scopes,
			"auth_path":     authPath,
			"is_enabled":    isEnabled,
			"created_at":    createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing OAuth providers: %w", err)
	}
	return tools.Ok(providers), nil
}

func epDeleteOAuth(ctx *tools.ToolContext, providerName, rawAuthPath string) (*tools.Result, error) {
	if providerName == "" {
		return tools.Fail("provider_name is required"), nil
	}
	authPath := tools.NormalizeEndpointPath(rawAuthPath)

	// Scope the delete to the auth endpoint the provider was attached to
	// (epCreateOAuth stores auth_endpoint_path), so removing a provider from one
	// endpoint never silently affects another.
	result, err := ctx.DB.Exec("DELETE FROM of_oauth_providers WHERE name = ? AND auth_endpoint_path = ?", providerName, authPath)
	if err != nil {
		return nil, fmt.Errorf("deleting OAuth provider: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return tools.Fail("OAuth provider '%s' not found on auth endpoint '%s'", providerName, authPath), nil
	}

	// Clean up the associated secret only after the provider row was removed.
	secretName := "oauth_" + providerName + "_secret"
	if _, err := ctx.DB.Exec("DELETE FROM of_secrets WHERE name = ?", secretName); err != nil {
		slog.Warn("failed to delete OAuth secret", "name", secretName, "error", err)
	}

	return tools.Ok(map[string]any{
		"deleted": providerName,
	}), nil
}

func epUpdateOAuth(ctx *tools.ToolContext, a *OAuthUpdate) (*tools.Result, error) {
	var exists int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_oauth_providers WHERE name = ?", a.ProviderName).Scan(&exists)
	if exists == 0 {
		return tools.FailCode(tools.CodeNotFound, "OAuth provider %q not found - register it with manage_oauth action=create", a.ProviderName), nil
	}
	sets := []string{}
	var vals []any
	for _, f := range []struct{ col, val string }{
		{"display_name", a.DisplayName}, {"client_id", a.ClientID}, {"authorize_url", a.AuthorizeURL},
		{"token_url", a.TokenURL}, {"userinfo_url", a.UserinfoURL}, {"scopes", a.Scopes},
		{"username_field", a.UsernameField},
	} {
		if f.val != "" {
			sets = append(sets, f.col+" = ?")
			vals = append(vals, f.val)
		}
	}
	if a.ClientSecret != "" {
		if ctx.Encryptor == nil {
			return tools.Fail("encryption not available"), nil
		}
		encrypted, err := ctx.Encryptor.Encrypt(a.ClientSecret)
		if err != nil {
			return tools.Fail("failed to encrypt client secret"), nil
		}
		secretName := "oauth_" + a.ProviderName + "_secret"
		if _, err := ctx.DB.Exec(
			`INSERT INTO of_secrets (name, value_encrypted) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value_encrypted = excluded.value_encrypted, updated_at = CURRENT_TIMESTAMP`,
			secretName, encrypted,
		); err != nil {
			return tools.Fail("failed to store encrypted secret: %v", err), nil
		}
		sets = append(sets, "client_secret_name = ?")
		vals = append(vals, secretName)
	}
	if len(sets) == 0 {
		return tools.Fail("nothing to update: pass at least one of client_id, client_secret, display_name, authorize_url, token_url, userinfo_url, scopes, username_field"), nil
	}
	vals = append(vals, a.ProviderName)
	if _, err := ctx.DB.Exec("UPDATE of_oauth_providers SET "+strings.Join(sets, ", ")+" WHERE name = ?", vals...); err != nil {
		return nil, fmt.Errorf("updating OAuth provider: %w", err)
	}
	return tools.Ok(map[string]any{"provider": a.ProviderName, "updated": true}), nil
}
