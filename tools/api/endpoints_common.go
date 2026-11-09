package api

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/internal/endpoints"
	"openfabrica/tools"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Shared helpers for the endpoint-management tools (manage_api, manage_auth,
// the realtime tools manage_relay/manage_tick/manage_authoritative/etc.,
// manage_sse, manage_llm_proxy). The per-tool tools.Define call and action
// handlers live in their respective manage_*.go files.

// titleCase applies English title-casing. A cases.Caser must not be shared between
// goroutines (x/text docs) and endpoint tools run concurrently, so build a fresh one
// per call rather than racing a package-level caser.
func titleCase(s string) string {
	return cases.Title(language.English).String(s)
}

// endpointTableKind maps a config table to its spine kind, so DeleteEndpointByPath
// can release the matching of_endpoints rows uniformly. Feeds are intentionally
// absent (separate namespace).
var endpointTableKind = map[string]string{
	"of_api_endpoints":        endpoints.KindCRUD,
	"of_function_endpoints":   endpoints.KindFunction,
	"of_query_endpoints":      endpoints.KindQuery,
	"of_auth_endpoints":       endpoints.KindAuth,
	"of_upload_endpoints":     endpoints.KindUpload,
	"of_stream_endpoints":     endpoints.KindSSE,
	"of_llm_endpoints":        endpoints.KindLLM,
	"of_agent_endpoints":      endpoints.KindAgent,
	"of_ws_endpoints":         endpoints.KindWS,
	"of_export_endpoints":     endpoints.KindExport,
	"of_import_endpoints":     endpoints.KindImport,
	"of_totp_endpoints":       endpoints.KindTOTP,
	"of_magic_link_endpoints": endpoints.KindMagic,
	"of_push_endpoints":       endpoints.KindPush,
	"of_document_endpoints":   endpoints.KindDocument,
}

// endpointExec is the subset of dbh.Handle and *sql.Tx that the spine claim
// uses, so a claim can run standalone or inside a create transaction.
type endpointExec interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

type endpointRoute struct{ SubSegment, Method string }

// endpointRoutesFor returns the (sub_segment, method) routes a kind claims at a
// base path. This is the SINGLE place the reserved sub-segments (upload, stream,
// ws, ...) are defined. methods matters only for kinds whose HTTP methods vary
// (crud, function); the rest have fixed routes. A method of "*" means the kind
// is the sole owner of that sub_segment regardless of method.
func endpointRoutesFor(kind string, methods []string) []endpointRoute {
	switch kind {
	case endpoints.KindCRUD, endpoints.KindFunction:
		rs := make([]endpointRoute, 0, len(methods))
		for _, m := range methods {
			rs = append(rs, endpointRoute{SubSegment: "", Method: strings.ToUpper(strings.TrimSpace(m))})
		}
		return rs
	case endpoints.KindQuery:
		return []endpointRoute{{"", "POST"}}
	case endpoints.KindUpload:
		return []endpointRoute{{"upload", "POST"}}
	case endpoints.KindSSE:
		return []endpointRoute{{"stream", "GET"}}
	case endpoints.KindLLM:
		return []endpointRoute{{"chat", "POST"}, {"complete", "POST"}}
	case endpoints.KindAgent:
		// POST /run starts a run; GET /run?id=<id> polls an async run.
		return []endpointRoute{{"run", "POST"}, {"run", "GET"}}
	case endpoints.KindWS:
		return []endpointRoute{{"ws", "*"}, {"rooms", "GET"}}
	case endpoints.KindExport:
		return []endpointRoute{{"export", "GET"}}
	case endpoints.KindImport:
		return []endpointRoute{{"import", "POST"}}
	case endpoints.KindPush:
		return []endpointRoute{{"push", "*"}}
	case endpoints.KindDocument:
		return []endpointRoute{{"pdf", "POST"}}
	case endpoints.KindTOTP:
		return []endpointRoute{{"totp", "*"}}
	case endpoints.KindMagic:
		return []endpointRoute{{"magic", "*"}}
	case endpoints.KindAuth:
		// Fixed auth action sub-routes; oauth/tenant params are dynamic families.
		return []endpointRoute{
			// No logout route: signing out is client-side (the auth helper's logout()
			// drops the token), so claiming one let a dead fetch to it pass api_target.
			{"register", "POST"}, {"login", "POST"},
			{"me", "GET"}, {"refresh", "POST"},
			{"oauth", "*"}, {"tenants", "*"}, {"tenant", "*"},
		}
	}
	return nil
}

// ClaimEndpoint makes the of_endpoints spine match the routes `kind` answers at
// `path`, enforcing the cross-kind uniqueness of (path, sub_segment, method). It
// is idempotent for the SAME kind (a build resume re-registers), and returns a
// collision error naming the other kind when any route is already owned by a
// DIFFERENT kind. Call it BEFORE writing the per-kind config row so a collision
// writes nothing.
//
// The claim is the kind's COMPLETE route set at the path, so routes this kind
// previously held and no longer claims are released here. That is what keeps a
// methods change (crud/function are the only kinds whose routes vary) from
// leaving the spine advertising a route the config row dropped, or missing one
// it gained. endpointRoutesFor returns the whole set in one call and no
// (kind, path) is claimed twice with different sets - of_api_endpoints.path and
// of_function_endpoints.path are UNIQUE - so the release can never drop a live
// route.
//
// Ordering is insert-before-release: the spine may transiently hold both the old
// and new routes, but it is never missing a live one. Releasing first would open
// a window where a request 404s.
func ClaimEndpoint(ctx *tools.ToolContext, kind, path string, methods []string) error {
	return claimEndpoint(ctx.DB, kind, path, methods)
}

// ClaimEndpointTx claims the spine on an open transaction, so a create can commit
// the spine row and its config row together (no half-created endpoint on failure).
func ClaimEndpointTx(tx *sql.Tx, kind, path string, methods []string) error {
	return claimEndpoint(tx, kind, path, methods)
}

func claimEndpoint(db endpointExec, kind, path string, methods []string) error {
	routes := endpointRoutesFor(kind, methods)

	// Reject a path whose last segment is the suffix the runtime appends for this
	// kind (an upload endpoint named "upload" is served at /api/upload/upload).
	// Naming it that invites a page to POST the bare /api/<path>, which no gate
	// catches. See the endpoint-routes contract.
	last := lastPathSegment(path)
	for _, rt := range routes {
		if rt.SubSegment != "" && rt.SubSegment == last {
			return fmt.Errorf("endpoint path %q collides with the runtime suffix: a %s endpoint is served at /api/<path>/%s, so this becomes /api/%s/%s. Pick a different path (the resource name).",
				last, kind, rt.SubSegment, path, rt.SubSegment)
		}
	}

	// Collision-check the whole set before writing anything: a rejected claim
	// must leave the spine exactly as it found it.
	for _, rt := range routes {
		var existingKind string
		err := db.QueryRow(
			"SELECT kind FROM of_endpoints WHERE path = ? AND sub_segment = ? AND method = ?",
			path, rt.SubSegment, rt.Method,
		).Scan(&existingKind)
		switch {
		case err == nil:
			if existingKind != kind {
				return fmt.Errorf("route %s is already owned by a %s endpoint - give this %s endpoint its own path (e.g. %s_%s)",
					routeLabel(path, rt), existingKind, kind, path, kind)
			}
		case err == sql.ErrNoRows:
			// free
		default:
			return err
		}
	}

	claimed := make(map[endpointRoute]bool, len(routes))
	for _, rt := range routes {
		claimed[rt] = true
		if _, err := db.Exec(
			`INSERT INTO of_endpoints (path, sub_segment, method, kind) VALUES (?, ?, ?, ?)
			 ON CONFLICT(path, sub_segment, method) DO NOTHING`,
			path, rt.SubSegment, rt.Method, kind,
		); err != nil {
			return err
		}
	}

	// Release this kind's rows at the path that the new set no longer claims.
	// Done in Go over the kind's own rows (at most a handful) rather than a
	// dynamic NOT IN.
	rows, err := db.Query(
		"SELECT sub_segment, method FROM of_endpoints WHERE path = ? AND kind = ?", path, kind)
	if err != nil {
		return err
	}
	var stale []endpointRoute
	for rows.Next() {
		var rt endpointRoute
		if err := rows.Scan(&rt.SubSegment, &rt.Method); err != nil {
			rows.Close()
			return err
		}
		if !claimed[rt] {
			stale = append(stale, rt)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, rt := range stale {
		if _, err := db.Exec(
			"DELETE FROM of_endpoints WHERE path = ? AND kind = ? AND sub_segment = ? AND method = ?",
			path, kind, rt.SubSegment, rt.Method,
		); err != nil {
			return err
		}
	}
	return nil
}

// lastPathSegment returns the final "/"-separated segment of an endpoint path.
func lastPathSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ReleaseEndpoint removes a kind's spine rows for a path (called on delete).
func ReleaseEndpoint(ctx *tools.ToolContext, kind, path string) error {
	_, err := ctx.DB.Exec("DELETE FROM of_endpoints WHERE path = ? AND kind = ?", path, kind)
	return err
}

// routeLabel renders a route as its public URL for error messages.
func routeLabel(path string, rt endpointRoute) string {
	m := ""
	if rt.Method != "*" {
		m = rt.Method + " "
	}
	if rt.SubSegment == "" {
		return fmt.Sprintf("%s/api/%s", m, path)
	}
	return fmt.Sprintf("%s/api/%s/%s", m, path, rt.SubSegment)
}

// DeleteEndpointByPath: validate path, delete by path on the given table, release
// the spine claim, and emit a destructive-action event.
func DeleteEndpointByPath(ctx *tools.ToolContext, table, entity, toolName, actionName, endpointPath string) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(endpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	res, err := ctx.DB.Exec("DELETE FROM "+table+" WHERE path = ?", path)
	if err != nil {
		return nil, fmt.Errorf("deleting %s: %w", entity, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.NotFound(entity, path), nil
	}
	if kind, ok := endpointTableKind[table]; ok {
		_ = ReleaseEndpoint(ctx, kind, path)
	}
	if toolName != "" && actionName != "" {
		tools.LogDestructiveAction(ctx.DB, toolName, actionName, path)
	}
	return tools.Ok(map[string]any{"deleted": path}), nil
}

// EpSummarize is the shared Summarize hook for the endpoint-management
// ToolDefs. Lists collapse to a count; create/update results surface the
// endpoint path; everything else falls back to a truncated tail.
func EpSummarize(result string) string {
	return tools.SummarizeFrame(result, "Listed %d endpoints", func(dataMap map[string]any) string {
		if path, _ := dataMap["endpoint_path"].(string); path != "" {
			return fmt.Sprintf(`{"success":true,"summary":"Endpoint at /api/%s"}`, path)
		}
		return ""
	})
}
