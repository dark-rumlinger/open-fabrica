package auth

import (
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/security"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupOAuthDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)

	stmts := []string{
		`INSERT INTO of_auth_endpoints (path, table_name) VALUES ('auth', 'users')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	return db
}

func newTestEncryptor(t *testing.T) *security.Encryptor {
	t.Helper()
	// 32-byte key in hex.
	enc, err := security.NewEncryptor("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatalf("encryptor: %v", err)
	}
	return enc
}

// TestOAuthCreateRoundTrip pins arg forwarding for manage_oauth create: every
// declared arg (including scopes and username_field) must reach the stored
// of_oauth_providers row, and the client secret must be encrypted into of_secrets.
func TestOAuthCreateRoundTrip(t *testing.T) {
	db := setupOAuthDB(t)
	ctx := &tools.ToolContext{DB: db, Encryptor: newTestEncryptor(t)}

	act := &OAuthCreate{
		ProviderName:  "github",
		DisplayName:   "Sign in with GitHub",
		ClientID:      "client-xyz",
		ClientSecret:  "super-secret-value",
		AuthorizeURL:  "https://github.com/login/oauth/authorize",
		TokenURL:      "https://github.com/login/oauth/access_token",
		UserinfoURL:   "https://api.github.com/user",
		Scopes:        "read:user user:email",
		UsernameField: "login",
		AuthPath:      "auth",
	}
	res, err := act.Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create oauth: err=%v res=%+v", err, res)
	}

	var displayName, clientID, secretName, authorizeURL, tokenURL, userinfoURL, scopes, usernameField, authPath string
	if err := db.QueryRow(
		`SELECT display_name, client_id, client_secret_name, authorize_url, token_url, userinfo_url, scopes, username_field, auth_endpoint_path
		 FROM of_oauth_providers WHERE name = ?`, "github",
	).Scan(&displayName, &clientID, &secretName, &authorizeURL, &tokenURL, &userinfoURL, &scopes, &usernameField, &authPath); err != nil {
		t.Fatalf("load provider row: %v", err)
	}
	if displayName != "Sign in with GitHub" {
		t.Errorf("display_name not stored: %q", displayName)
	}
	if clientID != "client-xyz" {
		t.Errorf("client_id not stored: %q", clientID)
	}
	if authorizeURL != "https://github.com/login/oauth/authorize" {
		t.Errorf("authorize_url not stored: %q", authorizeURL)
	}
	if tokenURL != "https://github.com/login/oauth/access_token" {
		t.Errorf("token_url not stored: %q", tokenURL)
	}
	if userinfoURL != "https://api.github.com/user" {
		t.Errorf("userinfo_url not stored: %q", userinfoURL)
	}
	if scopes != "read:user user:email" {
		t.Errorf("scopes not stored (arg-forwarding drift): %q", scopes)
	}
	if usernameField != "login" {
		t.Errorf("username_field not stored (arg-forwarding drift): %q", usernameField)
	}
	if authPath != "auth" {
		t.Errorf("auth_endpoint_path not stored: %q", authPath)
	}

	// The encrypted secret must round-trip back to the plaintext.
	var encrypted string
	if err := db.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = ?", secretName).Scan(&encrypted); err != nil {
		t.Fatalf("load secret: %v", err)
	}
	plain, err := ctx.Encryptor.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt secret: %v", err)
	}
	if plain != "super-secret-value" {
		t.Errorf("client_secret not stored/encrypted correctly: %q", plain)
	}
}

// TestOAuthDeleteRoundTrip pins delete removes the provider and its secret.
func TestOAuthDeleteRoundTrip(t *testing.T) {
	db := setupOAuthDB(t)
	ctx := &tools.ToolContext{DB: db, Encryptor: newTestEncryptor(t)}

	if _, err := (&OAuthCreate{
		ProviderName: "google", ClientID: "c", ClientSecret: "s",
		AuthorizeURL: "https://a", TokenURL: "https://t", UserinfoURL: "https://u",
		AuthPath: "auth",
	}).Run(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	del, err := (&OAuthDelete{AuthPath: "auth", ProviderName: "google"}).Run(ctx)
	if err != nil || del == nil || !del.Success {
		t.Fatalf("delete: err=%v res=%+v", err, del)
	}

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM of_oauth_providers WHERE name = ?", "google").Scan(&n); err != nil {
		t.Fatalf("count providers: %v", err)
	}
	if n != 0 {
		t.Errorf("provider not deleted, %d remain", n)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM of_secrets WHERE name = ?", "oauth_google_secret").Scan(&n); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if n != 0 {
		t.Errorf("secret not deleted, %d remain", n)
	}
}
