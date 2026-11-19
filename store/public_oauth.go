package store

import (
	"database/sql"
	"fmt"

	"openfabrica/security"
)

// OAuthProviderRow holds an enabled OAuth provider configuration from of_oauth_providers.
type OAuthProviderRow struct {
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

// LoadOAuthProvider loads an enabled OAuth provider by name and auth endpoint path.
func LoadOAuthProvider(reader *sql.DB, providerName, authPath string) (*OAuthProviderRow, error) {
	var op OAuthProviderRow
	err := reader.QueryRow(
		"SELECT id, name, display_name, client_id, client_secret_name, authorize_url, token_url, userinfo_url, scopes, username_field, auth_endpoint_path FROM of_oauth_providers WHERE name = ? AND auth_endpoint_path = ? AND is_enabled = 1",
		providerName, authPath,
	).Scan(
		&op.ID, &op.Name, &op.DisplayName, &op.ClientID, &op.ClientSecretName,
		&op.AuthorizeURL, &op.TokenURL, &op.UserinfoURL, &op.Scopes,
		&op.UsernameField, &op.AuthEndpointPath,
	)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// LoadSecretEncrypted reads value_encrypted from of_secrets by secret name.
func LoadSecretEncrypted(reader *sql.DB, name string) (string, error) {
	var enc string
	err := reader.QueryRow("SELECT value_encrypted FROM of_secrets WHERE name = ?", name).Scan(&enc)
	return enc, err
}

// FindOAuthUserByUsername looks up id and role for an OAuth username.
func FindOAuthUserByUsername(reader *sql.DB, physTable, usernameCol, roleCol, username string) (userID int, role string, err error) {
	var r sql.NullString
	err = reader.QueryRow(
		fmt.Sprintf("SELECT id, %s FROM %s WHERE %s = ?", security.QuoteIdent(roleCol), security.QuoteIdent(physTable), security.QuoteIdent(usernameCol)),
		username,
	).Scan(&userID, &r)
	return userID, r.String, err
}

// CreateOAuthUser inserts a new OAuth-provisioned user row and returns its id.
func CreateOAuthUser(writer *sql.DB, physTable, usernameCol, passwordCol, roleCol, username, passwordHash, defaultRole string) (int, error) {
	res, err := writer.Exec(
		fmt.Sprintf("INSERT INTO %s (%s, %s, %s) VALUES (?, ?, ?)", security.QuoteIdent(physTable), security.QuoteIdent(usernameCol), security.QuoteIdent(passwordCol), security.QuoteIdent(roleCol)),
		username, passwordHash, defaultRole,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return int(id), err
}
