package store

import (
	"database/sql"
	"sort"
	"strings"
)

// OwnerScopeAuthored is the of_api_endpoints.owner_scope value that makes
// owner_column scope writes only (reads stay shared across authenticated users).
// Single source of truth so the tool writing it and the runtime reading it cannot
// drift. An empty owner_scope keeps the default (scopes reads and writes alike).
const OwnerScopeAuthored = "authored"

// ownerLikeColumns are column-name patterns that usually hold a per-user or
// per-tenant owner reference: without owner_column/tenant_column the runtime
// applies no row filter and every caller reads (and writes) every row. Single
// source of truth: the save-time rls_warning nudge and the build-end rls_scope
// validator both decide through OwnerLikeColumn, so advisory and hard gate can never drift.
var ownerLikeColumns = map[string]bool{
	"user_id": true, "owner_id": true, "tenant_id": true, "account_id": true,
	"customer_id": true, "created_by": true, "author_id": true, "member_id": true,
}

// OwnerLikeColumn returns the first owner/tenant-like column in cols, or "" when
// none is present. Callers that pass columns in PRAGMA definition order get a
// deterministic pick, so the same table always reports the same column.
func OwnerLikeColumn(cols []string) string {
	for _, c := range cols {
		if ownerLikeColumns[strings.ToLower(c)] {
			return c
		}
	}
	return ""
}

// containerLikeColumns name a shared parent a row belongs to (a channel, a
// board). A row-owned table that ALSO carries one is usually collaborative
// content rather than private-per-user data, so a scope recommendation can lead
// with authored. Disjoint from ownerLikeColumns so the two picks never collide.
var containerLikeColumns = map[string]bool{
	"channel_id": true, "workspace_id": true, "room_id": true, "board_id": true,
	"conversation_id": true, "group_id": true, "team_id": true, "thread_id": true,
	"space_id": true, "org_id": true, "organization_id": true, "list_id": true,
	"project_id": true, "message_id": true, "post_id": true,
}

// ContainerColumn returns the first container-like column in cols, or "" when none
// is present. Definition-order like OwnerLikeColumn for a deterministic pick.
func ContainerColumn(cols []string) string {
	for _, c := range cols {
		if containerLikeColumns[strings.ToLower(c)] {
			return c
		}
	}
	return ""
}

// OwnerScopeFinding is one writable endpoint whose backing table looks row-owned
// but which applies no row-level scope.
type OwnerScopeFinding struct {
	Path         string // endpoint path (served at /api/<path>)
	Table        string // backing dynamic table
	Column       string // the owner-like column left unscoped
	Container    string // a container-like column (channel_id/workspace_id/...) or "" - marks likely-shared content
	PublicRead   bool   // GET is public - read exposes every row to anyone
	RequiresAuth bool   // endpoint requires auth - the leak is between logged-in users
}

// UnscopedOwnerEndpoints returns every writable API endpoint whose backing table
// carries an owner/tenant-like column but sets neither owner_column nor
// tenant_column - every caller sees every row (a cross-user / IDOR exposure).
// Build-end hard-gate counterpart to the save-time rls_warning advisory; pure DB
// reads, so it runs in the no-browser static tier. read_only (view-backed)
// endpoints are skipped; an introspection failure yields no finding rather than a spurious one.
func UnscopedOwnerEndpoints(db *sql.DB) []OwnerScopeFinding {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`
		SELECT path, table_name,
		       COALESCE(requires_auth, 0), COALESCE(public_read, 0),
		       COALESCE(owner_column, ''), COALESCE(tenant_column, ''),
		       COALESCE(read_only, 0), COALESCE(public_write, 0)
		FROM of_api_endpoints`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type endpoint struct {
		path, table  string
		requiresAuth bool
		publicRead   bool
	}
	var candidates []endpoint
	for rows.Next() {
		var path, table, owner, tenant string
		var requiresAuth, publicRead, readOnly, publicWrite int
		if err := rows.Scan(&path, &table, &requiresAuth, &publicRead, &owner, &tenant, &readOnly, &publicWrite); err != nil {
			continue
		}
		// Already scoped, view-backed (no RLS surface), or no backing table: nothing
		// to flag. public_write endpoints are also skipped: writes are anonymous and
		// reads admin-gated, so there is no cross-user leak - owner-scoping them
		// would be invalid.
		if owner != "" || tenant != "" || readOnly == 1 || table == "" || publicWrite == 1 {
			continue
		}
		candidates = append(candidates, endpoint{
			path: path, table: table,
			requiresAuth: requiresAuth == 1, publicRead: publicRead == 1,
		})
	}
	if rows.Err() != nil || len(candidates) == 0 {
		return nil
	}

	var out []OwnerScopeFinding
	for _, e := range candidates {
		cols := TableColumnNames(db, e.table)
		col := OwnerLikeColumn(cols)
		if col == "" {
			continue
		}
		out = append(out, OwnerScopeFinding{
			Path:         e.path,
			Table:        e.table,
			Column:       col,
			Container:    ContainerColumn(cols),
			PublicRead:   e.publicRead,
			RequiresAuth: e.requiresAuth,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
