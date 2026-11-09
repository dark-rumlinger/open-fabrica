package models

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type User struct {
	ID           int       `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	DisplayName  string    `json:"display_name"`
	Role         string    `json:"role"`
	TokenVersion int       `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func CreateUser(db *sql.DB, username, passwordHash, role, displayName string) (*User, error) {
	return CreateUserTx(db, username, passwordHash, role, displayName)
}

// CreateUserTx creates a user using any Executor (DB or Tx).
func CreateUserTx(exec Executor, username, passwordHash, role, displayName string) (*User, error) {
	result, err := exec.Exec(
		"INSERT INTO users (username, password_hash, role, display_name) VALUES (?, ?, ?, ?)",
		username, passwordHash, role, displayName,
	)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}
	return &User{
		ID:          int(id),
		Username:    username,
		DisplayName: displayName,
		Role:        role,
	}, nil
}

// getUserBy selects one full user row by the given column. The column is a
// compile-time constant ("username" or "id") at its only call sites, never
// caller input, so it cannot be injected.
func getUserBy(db *sql.DB, column string, value any) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		"SELECT id, username, password_hash, display_name, role, token_version, created_at, updated_at FROM users WHERE "+column+" = ?",
		value,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.TokenVersion, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func GetUserByUsername(db *sql.DB, username string) (*User, error) {
	return getUserBy(db, "username", username)
}

func GetUserByID(db *sql.DB, id int) (*User, error) {
	return getUserBy(db, "id", id)
}

// GetUserTokenVersion returns the current token_version for a user. Used by the
// admin auth guard to reject JWTs minted before the version was bumped.
func GetUserTokenVersion(db *sql.DB, id int) (int, error) {
	var v int
	err := db.QueryRow("SELECT token_version FROM users WHERE id = ?", id).Scan(&v)
	return v, err
}

func ListUsers(db *sql.DB) ([]User, error) {
	rows, err := db.Query("SELECT id, username, display_name, role, created_at, updated_at FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func CountUsers(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

// UpdateUserPassword changes the password hash and bumps token_version in the
// same statement, so changing a password immediately invalidates every JWT
// issued before the change.
func UpdateUserPassword(db *sql.DB, id int, passwordHash string) error {
	_, err := db.Exec(
		"UPDATE users SET password_hash = ?, token_version = token_version + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		passwordHash, id,
	)
	return err
}

// BumpUserTokenVersion invalidates every JWT issued to the user so far (sign out
// everywhere). Tokens minted afterwards carry the new version.
func BumpUserTokenVersion(db *sql.DB, id int) error {
	_, err := db.Exec(
		"UPDATE users SET token_version = token_version + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		id,
	)
	return err
}

// UpdateUserRole changes a role and bumps token_version in the same statement, so
// a demoted admin's existing JWTs stop working at once instead of keeping admin
// rights until they expire.
func UpdateUserRole(db *sql.DB, id int, role string) error {
	_, err := db.Exec(
		"UPDATE users SET role = ?, token_version = token_version + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		role, id,
	)
	return err
}

func UpdateUserDisplayName(db *sql.DB, id int, displayName string) error {
	_, err := db.Exec(
		"UPDATE users SET display_name = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		displayName, id,
	)
	return err
}

func DeleteUser(db *sql.DB, id int) error {
	_, err := db.Exec("DELETE FROM users WHERE id = ?", id)
	return err
}

// GetUserAssistantLanguage returns the user's stored assistant-language
// preference, "" (follow the interface language) when they have never set one.
// The value is stored as given; replylang owns what it may contain.
func GetUserAssistantLanguage(db *sql.DB, userID int) (string, error) {
	var lang string
	err := db.QueryRow("SELECT assistant_language FROM user_preferences WHERE user_id = ?", userID).Scan(&lang)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return lang, err
}

// SetUserAssistantLanguage stores the user's assistant-language preference. It
// fails for a user that does not exist rather than leaving an orphan row.
func SetUserAssistantLanguage(db *sql.DB, userID int, lang string) error {
	_, err := db.Exec(
		`INSERT INTO user_preferences (user_id, assistant_language) VALUES (?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET assistant_language = excluded.assistant_language, updated_at = CURRENT_TIMESTAMP`,
		userID, lang,
	)
	return err
}
