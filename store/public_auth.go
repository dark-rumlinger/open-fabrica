package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"openfabrica/security"
)

// PublicAuthEndpointRow holds the raw configuration columns from of_auth_endpoints.
type PublicAuthEndpointRow struct {
	ID               int
	TableName        string
	Path             string
	UsernameColumn   string
	PasswordColumn   string
	PublicColsJSON   string
	SignupColsJSON   string
	JWTExpiryHours   int
	DefaultRole      string
	RoleColumn       string
	RateLimit        int
	TenantSignupMode string
}

// LoadPublicAuthEndpoint loads an auth endpoint row from of_auth_endpoints by path.
func LoadPublicAuthEndpoint(reader *sql.DB, path string) (*PublicAuthEndpointRow, error) {
	var r PublicAuthEndpointRow
	err := reader.QueryRow(
		"SELECT id, table_name, path, username_column, password_column, public_columns, COALESCE(signup_columns,'[]'), jwt_expiry_hours, default_role, role_column, COALESCE(rate_limit,60), COALESCE(tenant_signup_mode,'none') FROM of_auth_endpoints WHERE path = ?",
		path,
	).Scan(
		&r.ID, &r.TableName, &r.Path, &r.UsernameColumn, &r.PasswordColumn,
		&r.PublicColsJSON, &r.SignupColsJSON, &r.JWTExpiryHours, &r.DefaultRole,
		&r.RoleColumn, &r.RateLimit, &r.TenantSignupMode,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// FindAuthUserIDByUsername looks up a user's id in a dynamic auth table.
func FindAuthUserIDByUsername(reader *sql.DB, physTable, usernameCol, username string) (int, error) {
	var id int
	err := reader.QueryRow(
		fmt.Sprintf("SELECT id FROM %s WHERE %s = ?", security.QuoteIdent(physTable), security.QuoteIdent(usernameCol)),
		username,
	).Scan(&id)
	return id, err
}

// FindAuthUserCredentials looks up id, password hash, and role by username.
func FindAuthUserCredentials(reader *sql.DB, physTable, usernameCol, passwordCol, roleCol, username string) (userID int, hashedPassword, role string, err error) {
	var r sql.NullString
	err = reader.QueryRow(
		fmt.Sprintf("SELECT id, %s, %s FROM %s WHERE %s = ?", security.QuoteIdent(passwordCol), security.QuoteIdent(roleCol), security.QuoteIdent(physTable), security.QuoteIdent(usernameCol)),
		username,
	).Scan(&userID, &hashedPassword, &r)
	return userID, hashedPassword, r.String, err
}

// InsertAuthUser inserts a new user row into a dynamic auth table and returns its id.
func InsertAuthUser(writer *sql.DB, physTable string, quotedCols []string, values []any) (int64, error) {
	placeholders := make([]string, len(quotedCols))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", security.QuoteIdent(physTable), strings.Join(quotedCols, ", "), strings.Join(placeholders, ", "))
	res, err := writer.Exec(query, values...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteAuthUserByID removes a user row by id from a dynamic auth table.
func DeleteAuthUserByID(writer *sql.DB, physTable string, userID int64) error {
	_, err := writer.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", security.QuoteIdent(physTable)), userID)
	return err
}

// LoadAuthUserMe loads the visible columns for a user's /me profile.
func LoadAuthUserMe(reader *sql.DB, physTable string, quotedCols []string, userID int) (map[string]any, error) {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", strings.Join(quotedCols, ", "), security.QuoteIdent(physTable))
	rows, err := reader.Query(query, userID)
	if err != nil {
		return nil, err
	}
	results := ScanRowsToMaps(rows)
	if len(results) == 0 {
		return nil, sql.ErrNoRows
	}
	return results[0], nil
}

// AuthUserExists reports whether a user row with userID still exists.
func AuthUserExists(reader *sql.DB, physTable string, userID int) bool {
	var exists int
	err := reader.QueryRow(
		fmt.Sprintf("SELECT 1 FROM %s WHERE id = ?", security.QuoteIdent(physTable)),
		userID,
	).Scan(&exists)
	return err == nil
}

// ReadAuthUserRole reads a user's role column, falling back to defaultRole.
func ReadAuthUserRole(reader *sql.DB, physTable, roleCol string, userID int, defaultRole string) string {
	var role sql.NullString
	_ = reader.QueryRow(
		fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", security.QuoteIdent(roleCol), security.QuoteIdent(physTable)),
		userID,
	).Scan(&role)
	if role.String == "" {
		return defaultRole
	}
	return role.String
}

// ReadAuthUsername reads a user's username column, returning "" if missing.
func ReadAuthUsername(reader *sql.DB, physTable, usernameCol string, userID int) string {
	var name string
	_ = reader.QueryRow(
		fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", security.QuoteIdent(usernameCol), security.QuoteIdent(physTable)),
		userID,
	).Scan(&name)
	return name
}

// UpdateAuthUserPassword updates the password hash column for userID.
func UpdateAuthUserPassword(writer *sql.DB, physTable, passwordCol string, userID int64, hashedPassword string) error {
	_, err := writer.Exec(
		fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", security.QuoteIdent(physTable), security.QuoteIdent(passwordCol)),
		hashedPassword, userID,
	)
	return err
}

// GetTOTPEndpointConfig returns the issuer and enforced flag for a TOTP endpoint.
func GetTOTPEndpointConfig(reader *sql.DB, path string) (issuer string, enforced bool, err error) {
	err = reader.QueryRow("SELECT issuer, enforced FROM of_totp_endpoints WHERE path = ?", path).Scan(&issuer, &enforced)
	return issuer, enforced, err
}

// GetUserTOTP reads the encrypted secret and verified status for userID.
func GetUserTOTP(reader *sql.DB, userID int) (secretEnc string, verified bool, err error) {
	err = reader.QueryRow("SELECT secret_encrypted, verified FROM of_user_totp WHERE user_id = ?", userID).Scan(&secretEnc, &verified)
	return secretEnc, verified, err
}

// UpsertUserTOTP stores or replaces an unverified TOTP secret for userID.
func UpsertUserTOTP(writer *sql.DB, userID int, authPath, secretEnc string) error {
	_, err := writer.Exec(`
		INSERT INTO of_user_totp (user_id, auth_endpoint_path, secret_encrypted, verified)
		VALUES (?, ?, ?, 0)
		ON CONFLICT(user_id) DO UPDATE SET secret_encrypted = excluded.secret_encrypted, verified = 0`,
		userID, authPath, secretEnc,
	)
	return err
}

// MarkUserTOTPVerified marks a user's TOTP secret as verified.
func MarkUserTOTPVerified(writer *sql.DB, userID int) error {
	_, err := writer.Exec("UPDATE of_user_totp SET verified = 1, verified_at = CURRENT_TIMESTAMP WHERE user_id = ?", userID)
	return err
}

// DeleteUserTOTP removes a user's TOTP enrollment.
func DeleteUserTOTP(writer *sql.DB, userID int) error {
	_, err := writer.Exec("DELETE FROM of_user_totp WHERE user_id = ?", userID)
	return err
}

// MagicLinkEndpointConfig holds settings from of_magic_link_endpoints.
type MagicLinkEndpointConfig struct {
	EmailTemplate string
	RedirectURL   string
	TTLMinutes    int
}

// LoadMagicLinkEndpoint loads magic-link settings for an auth endpoint path.
func LoadMagicLinkEndpoint(reader *sql.DB, path string) (*MagicLinkEndpointConfig, error) {
	var cfg MagicLinkEndpointConfig
	err := reader.QueryRow(
		"SELECT email_template, redirect_url, ttl_minutes FROM of_magic_link_endpoints WHERE path = ?",
		path,
	).Scan(&cfg.EmailTemplate, &cfg.RedirectURL, &cfg.TTLMinutes)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// MagicTokenRow holds a row from of_magic_tokens.
type MagicTokenRow struct {
	Email     string
	UserID    sql.NullInt64
	ExpiresAt time.Time
	Used      bool
	Purpose   string
	Method    string
}

// PruneExpiredMagicTokens deletes magic tokens older than 1 day (best-effort).
func PruneExpiredMagicTokens(writer *sql.DB) {
	bestEffortExec(writer, "prune expired magic tokens", "DELETE FROM of_magic_tokens WHERE created_at < datetime('now', '-1 day')")
}

// InsertMagicLoginToken inserts a login magic token.
func InsertMagicLoginToken(writer *sql.DB, tokenHash, authPath, email string, userID sql.NullInt64, expiresAt time.Time) error {
	_, err := writer.Exec(`
		INSERT INTO of_magic_tokens (token, auth_endpoint_path, email, user_id, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		tokenHash, authPath, email, userID, expiresAt,
	)
	return err
}

// ReplaceUserMagicToken deletes earlier unused tokens of the same purpose/user/path and inserts a new one.
func ReplaceUserMagicToken(writer *sql.DB, tokenHash, authPath, email string, userID int, expiresAt time.Time, purpose, method string) error {
	bestEffortExec(writer, "clear prior magic token", "DELETE FROM of_magic_tokens WHERE purpose = ? AND user_id = ? AND auth_endpoint_path = ?", purpose, userID, authPath)
	if method != "" {
		_, err := writer.Exec(
			`INSERT INTO of_magic_tokens (token, auth_endpoint_path, email, user_id, expires_at, purpose, method) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			tokenHash, authPath, email, userID, expiresAt, purpose, method,
		)
		return err
	}
	_, err := writer.Exec(
		`INSERT INTO of_magic_tokens (token, auth_endpoint_path, email, user_id, expires_at, purpose) VALUES (?, ?, ?, ?, ?, ?)`,
		tokenHash, authPath, email, userID, expiresAt, purpose,
	)
	return err
}

// LookupMagicToken looks up a magic token matching tokenHash, authPath, and any of purposes.
func LookupMagicToken(reader *sql.DB, tokenHash, authPath string, purposes ...string) (*MagicTokenRow, error) {
	if len(purposes) == 0 {
		return nil, sql.ErrNoRows
	}
	placeholders := make([]string, len(purposes))
	args := make([]any, 0, 2+len(purposes))
	args = append(args, tokenHash, authPath)
	for i, p := range purposes {
		placeholders[i] = "?"
		args = append(args, p)
	}
	var r MagicTokenRow
	query := fmt.Sprintf(
		`SELECT email, user_id, expires_at, used, purpose, COALESCE(method, '')
		 FROM of_magic_tokens WHERE token = ? AND auth_endpoint_path = ? AND purpose IN (%s)`,
		strings.Join(placeholders, ", "),
	)
	err := reader.QueryRow(query, args...).Scan(&r.Email, &r.UserID, &r.ExpiresAt, &r.Used, &r.Purpose, &r.Method)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ConsumeMagicToken atomically marks a magic token as used, reporting whether it was unused.
func ConsumeMagicToken(writer *sql.DB, tokenHash string) (bool, error) {
	res, err := writer.Exec("UPDATE of_magic_tokens SET used = 1 WHERE token = ? AND used = 0", tokenHash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UnconsumeMagicToken resets used = 0 on a magic token after a downstream failure (best-effort).
func UnconsumeMagicToken(writer *sql.DB, tokenHash string) {
	bestEffortExec(writer, "unconsume magic token", "UPDATE of_magic_tokens SET used = 0 WHERE token = ?", tokenHash)
}

// LoadEmailTemplate loads subject and body_html from of_email_templates by name.
func LoadEmailTemplate(reader *sql.DB, name string) (subject, bodyHTML string) {
	_ = reader.QueryRow("SELECT subject, body_html FROM of_email_templates WHERE name = ?", name).Scan(&subject, &bodyHTML)
	return subject, bodyHTML
}

// EmailConfigCount returns the number of rows in of_email_config.
func EmailConfigCount(reader *sql.DB) int {
	var n int
	_ = reader.QueryRow("SELECT COUNT(*) FROM of_email_config").Scan(&n)
	return n
}

// EnqueueSendEmailJob inserts a pending send_email job into of_jobs.
func EnqueueSendEmailJob(writer *sql.DB, payload string) error {
	_, err := writer.Exec(`INSERT INTO of_jobs (type, payload, status, max_attempts) VALUES ('send_email', ?, 'pending', 3)`, payload)
	return err
}
