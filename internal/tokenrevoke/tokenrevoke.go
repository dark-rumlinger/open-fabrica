// Package tokenrevoke ends the sessions of a project's own users before their
// tokens expire.
//
// The sign-in tokens of the apps built here are stateless signed JWTs, valid until
// they expire (24 hours by default). A password reset, a deleted or banned user or
// a changed role must not leave tokens issued earlier working, so each project
// database records, per user, an instant: tokens issued before it are refused.
// Nothing about a token has to change, which is why this works for every place that
// mints one.
//
// Only users who have been revoked have a row, so the table stays small. It is read
// into memory once per project and kept current by Revoke, so checking a request
// costs a map lookup, not a query.
package tokenrevoke

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"openfabrica/security"
)

// Store holds the revocation instants of every project in the process.
type Store struct {
	mu     sync.RWMutex
	loaded map[int]bool
	before map[int]map[int]int64 // project id -> user id -> unix milliseconds
}

// New returns an empty store.
func New() *Store {
	return &Store{loaded: map[int]bool{}, before: map[int]map[int]int64{}}
}

var defaultStore = New()

// Revoke ends every session the user has now: tokens issued before this call stop
// working, tokens issued after it do. w must be the project's write handle.
func Revoke(w *sql.DB, projectID, userID int) error {
	return defaultStore.Revoke(w, projectID, userID, time.Now())
}

// Revoked reports whether a token issued at issuedAt for the user predates a
// revocation. r is the project's read handle. A token with no issue time cannot be
// shown to postdate one, so it counts as revoked once the user has any revocation.
func Revoked(r *sql.DB, projectID, userID int, issuedAt *time.Time) bool {
	return defaultStore.Revoked(r, projectID, userID, issuedAt)
}

// Forget drops a project's cached state, for when the project is deleted.
func Forget(projectID int) { defaultStore.Forget(projectID) }

// Revoke records the revocation and updates the in-memory view.
func (s *Store) Revoke(w *sql.DB, projectID, userID int, at time.Time) error {
	ms := at.UnixMilli()
	if _, err := w.Exec(
		`INSERT INTO of_token_revocations (user_id, revoked_before) VALUES (?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET revoked_before = excluded.revoked_before`,
		userID, ms,
	); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.before[projectID] == nil {
		s.before[projectID] = map[int]int64{}
	}
	s.before[projectID][userID] = ms
	return nil
}

// Revoked implements the package-level Revoked.
func (s *Store) Revoked(r *sql.DB, projectID, userID int, issuedAt *time.Time) bool {
	s.ensureLoaded(r, projectID)
	s.mu.RLock()
	at, ok := s.before[projectID][userID]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	return issuedAt == nil || issuedAt.UnixMilli() < at
}

// Forget implements the package-level Forget.
func (s *Store) Forget(projectID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.loaded, projectID)
	delete(s.before, projectID)
}

// ensureLoaded reads the project's rows the first time it is asked about. A
// database without the table has no revocations. Any other read failure leaves the
// project unloaded, so the next request tries again, and lets this one through: a
// transient lock must not sign everyone out.
func (s *Store) ensureLoaded(r *sql.DB, projectID int) {
	s.mu.RLock()
	done := s.loaded[projectID]
	s.mu.RUnlock()
	if done {
		return
	}

	rows, err := r.Query(`SELECT user_id, revoked_before FROM of_token_revocations`)
	loaded := map[int]int64{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var user int
			var ms int64
			if rows.Scan(&user, &ms) == nil {
				loaded[user] = ms
			}
		}
		err = rows.Err()
	}
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		slog.Warn("token revocations could not be read; sessions are not being checked until they can", "project_id", projectID, "error", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded[projectID] {
		return // another request loaded it first
	}
	merged := s.before[projectID] // a Revoke that raced the load wins
	if merged == nil {
		merged = map[int]int64{}
	}
	for user, ms := range loaded {
		if cur, ok := merged[user]; !ok || ms > cur {
			merged[user] = ms
		}
	}
	s.before[projectID] = merged
	s.loaded[projectID] = true
}

// AccountStandingColumns are column names that grant rights or standing to an
// account: a visitor registering must never set them, and changing one of them on an
// existing account ends that account's sessions, since a token carries the rights
// the account had when it was issued.
var AccountStandingColumns = map[string]bool{
	"is_admin": true, "admin": true, "is_staff": true, "is_superuser": true,
	"verified": true, "email_verified": true, "is_verified": true,
	"credits": true, "balance": true, "plan": true, "is_premium": true,
	"banned": true, "is_active": true,
}

// AccountToRevoke names the user whose sessions a write to one row of a table should
// end, or 0 when it should end none. The table must be an auth endpoint's account
// table. A delete (changed == nil) always ends them; an update ends them only when it
// touches a column that decides what the account may do or whether it may sign in:
// the password, the role, or a standing flag such as banned or is_active. Editing a
// display name must not sign the person out. Call it BEFORE the write, so a delete
// or an update of the lookup column itself can still be resolved to a user.
func AccountToRevoke(projectDB *sql.DB, physTable, lookupCol string, lookupVal any, changed []string) int {
	var passwordCol, roleCol string
	if err := projectDB.QueryRow(
		"SELECT password_column, role_column FROM of_auth_endpoints WHERE table_name = ? LIMIT 1", physTable,
	).Scan(&passwordCol, &roleCol); err != nil {
		return 0 // not an account table
	}
	if changed != nil {
		decides := false
		for _, col := range changed {
			c := strings.ToLower(col)
			if c == strings.ToLower(passwordCol) || c == strings.ToLower(roleCol) || AccountStandingColumns[c] {
				decides = true
				break
			}
		}
		if !decides {
			return 0
		}
	}
	var id int
	if err := projectDB.QueryRow(
		fmt.Sprintf("SELECT id FROM %s WHERE %s = ?", security.QuoteIdent(physTable), security.QuoteIdent(lookupCol)), lookupVal,
	).Scan(&id); err != nil {
		return 0
	}
	return id
}
