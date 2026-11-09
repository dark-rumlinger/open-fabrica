package store

import (
	"database/sql"
	"sort"

	"openfabrica/internal/dbh"
)

// Auth-gate parity: an auth-requiring API endpoint in a project with NO login
// surface 401s every write from the browser - dead on arrival. This is the
// single source of truth for "does the project have a login?", shared by the
// save-time auth_warning nudge (tools/api) and the build-end auth_gate
// validator (engine), so the advisory and the hard gate can never drift.

// ProjectHasAuth reports whether the project exposes any user-login surface, i.e.
// at least one row in of_auth_endpoints (a create_auth / create_oauth ran).
// Best-effort: a query error reads as "has auth" so a missing/odd schema never
// produces a spurious warning.
func ProjectHasAuth(db dbh.Handle) bool {
	if db == nil {
		return true
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM of_auth_endpoints").Scan(&n); err != nil {
		return true
	}
	return n > 0
}

// AuthGateFinding is one API endpoint that requires user auth while the project
// has no login surface, so it 401s from the browser.
type AuthGateFinding struct {
	Path       string // endpoint path (served at /api/<path>)
	PublicRead bool   // GET stays open; only writes 401
	// OwnerColumn is non-empty when the endpoint was PLANNED owner-scoped (per-user
	// rows). It changes the remedy: for an owner-scoped endpoint the fix is to add a
	// login, not to flip it public - public would discard the per-user ownership and
	// expose every row to every visitor.
	OwnerColumn string
}

// AuthGatedEndpointsWithoutAuth returns every API endpoint that requires user
// auth when the project has no login surface - a guaranteed 401 on write. It is
// the build-end counterpart to the save-time auth_warning, both deciding via
// ProjectHasAuth. Pure DB reads, so it runs in the no-browser static tier.
// Best-effort: an introspection failure yields no finding rather than a
// spurious one.
func AuthGatedEndpointsWithoutAuth(db *sql.DB) []AuthGateFinding {
	if db == nil || ProjectHasAuth(db) {
		return nil
	}
	// public_write endpoints are excluded: their write surface is intentionally
	// open, so writes do NOT 401. Flagging them would push the gated admin reads
	// open, re-introducing the very leak public_write exists to prevent; the
	// gated reads needing a future admin login is by design.
	rows, err := db.Query("SELECT path, COALESCE(public_read,0), COALESCE(owner_column,'') FROM of_api_endpoints WHERE COALESCE(requires_auth,0) = 1 AND COALESCE(public_write,0) = 0")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []AuthGateFinding
	for rows.Next() {
		var path, ownerColumn string
		var publicRead int
		if err := rows.Scan(&path, &publicRead, &ownerColumn); err != nil {
			continue
		}
		out = append(out, AuthGateFinding{Path: path, PublicRead: publicRead == 1, OwnerColumn: ownerColumn})
	}
	if rows.Err() != nil {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
