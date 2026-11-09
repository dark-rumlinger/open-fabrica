package snapshotbuild

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AuthKind values for EndpointRef.AuthKind: the kind of endpoint a snapshot row
// describes. Named so the assignment and comparison sites can't drift on a typo.
const (
	authKindAPI    = "api"
	authKindAuth   = "auth"
	authKindWS     = "ws"
	authKindStream = "stream"
	authKindLLM    = "llm"
	authKindUpload = "upload"
)

// WSMode values for EndpointRef.WSMode: the delivery model behind a "WS" row. Transport is
// the same for all four; what differs is the client API and what the server does with a
// frame, so the mode separates two WS endpoints a path alone renders identically.
const (
	wsModeRelay         = "relay"
	wsModeTick          = "tick"
	wsModeMatchmaking   = "matchmaking"
	wsModeAuthoritative = "authoritative"
)

// EndpointRef is a single endpoint entry in the project snapshot.
type EndpointRef struct {
	Path     string
	Methods  string // e.g. "GET,POST"
	Auth     bool
	// PublicWrite reports a REST endpoint that allows anonymous writes even while Auth
	// is set (the public-submission shape: anyone may POST, reads stay gated). Only
	// meaningful for authKindAPI; false elsewhere.
	PublicWrite bool
	AuthKind    string // one of the authKind* constants
	// WSMode is one of the wsMode* constants on an authKindWS row, "" elsewhere.
	WSMode string
	// TickHz is the server-driven rate on a tick (snapshot coalescing) or
	// authoritative (@tick sim) endpoint; 0 otherwise. The two are different
	// meanings for one number, which is why WSMode has to be read alongside it.
	TickHz int
	// HasReducer reports whether an authoritative endpoint's reducer is attached. Creating
	// the endpoint and attaching the reducer are two separate calls, and an authoritative
	// endpoint without one is inert (accepts connections, applies nothing). Presence, not
	// size (see the fingerprint note in entityList for why a bit, not a length).
	HasReducer bool
}

// wsTag renders what one WS endpoint has that another does not: its mode, server rate, and
// whether an authoritative endpoint's reducer is there. The missing-reducer case names the
// tool that fixes it, because it is the one state here that is always a bug.
func wsTag(e EndpointRef) string {
	parts := []string{e.WSMode}
	if e.TickHz > 0 {
		parts = append(parts, fmt.Sprintf("%dHz", e.TickHz))
	}
	if e.WSMode == wsModeAuthoritative {
		if e.HasReducer {
			parts = append(parts, "reducer attached")
		} else {
			parts = append(parts, "NO REDUCER - attach one with manage_authoritative set_reducer")
		}
	}
	return strings.Join(parts, ", ")
}

// wsMode collapses the endpoint's mode columns into the single platform name. Authoritative
// outranks matchmaking (the two cannot combine on one endpoint), and both outrank a tick
// rate: tick_hz on an authoritative endpoint is a SIM rate driving @tick into reduce(), not
// the coalescing snapshot tick, so the mode is decided before the rate reads.
func wsMode(authoritative, matchmaking bool, tickHz int) string {
	switch {
	case authoritative:
		return wsModeAuthoritative
	case matchmaking:
		return wsModeMatchmaking
	case tickHz > 0:
		return wsModeTick
	}
	return wsModeRelay
}

// loadEndpoints reads every kind of project endpoint - REST, auth, WebSocket, SSE,
// LLM, and upload - into one list, in that order.
func loadEndpoints(ctx context.Context, db *sql.DB) []EndpointRef {
	var out []EndpointRef

	if rows, err := db.QueryContext(ctx,
		`SELECT path, COALESCE(methods,''), COALESCE(requires_auth,0), COALESCE(public_write,0) FROM of_api_endpoints ORDER BY path`); err == nil {
		for rows.Next() {
			var p, methods string
			var auth, publicWrite bool
			if err := rows.Scan(&p, &methods, &auth, &publicWrite); err == nil {
				out = append(out, EndpointRef{Path: p, Methods: cleanMethods(methods), Auth: auth, PublicWrite: publicWrite, AuthKind: authKindAPI})
			}
		}
		_ = rows.Err()
		rows.Close()
	}

	if rows, err := db.QueryContext(ctx,
		`SELECT path FROM of_auth_endpoints ORDER BY path`); err == nil {
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err == nil {
				out = append(out, EndpointRef{Path: p, Methods: "POST", Auth: true, AuthKind: authKindAuth})
			}
		}
		_ = rows.Err()
		rows.Close()
	}

	// WebSocket endpoints, served at /api/{path}/ws. The mode columns come along because
	// "WS <path>" is otherwise the same row for a chat relay and a reducer-backed game, and
	// the model cannot open a reducer to find out. LENGTH() rather than the body: presence is
	// the fact, and a reducer is the largest single value in the project.
	if rows, err := db.QueryContext(ctx,
		`SELECT path, COALESCE(requires_auth,0), COALESCE(authoritative,0), COALESCE(matchmaking,0),
		        COALESCE(tick_hz,0), LENGTH(COALESCE(reducer,''))
		   FROM of_ws_endpoints ORDER BY path`); err == nil {
		for rows.Next() {
			var p string
			var auth, authoritative, matchmaking bool
			var tickHz, reducerLen int
			if err := rows.Scan(&p, &auth, &authoritative, &matchmaking, &tickHz, &reducerLen); err == nil {
				out = append(out, EndpointRef{
					Path: p, Methods: "WS", Auth: auth, AuthKind: authKindWS,
					WSMode:     wsMode(authoritative, matchmaking, tickHz),
					TickHz:     tickHz,
					HasReducer: reducerLen > 0,
				})
			}
		}
		_ = rows.Err()
		rows.Close()
	}

	// SSE, LLM, and upload rows are each just a path plus an auth flag.
	out = collectSimpleEndpoints(ctx, db, out,
		`SELECT path, COALESCE(requires_auth,0) FROM of_stream_endpoints ORDER BY path`, "SSE", authKindStream)
	out = collectSimpleEndpoints(ctx, db, out,
		`SELECT path, COALESCE(requires_auth,0) FROM of_llm_endpoints ORDER BY path`, "POST", authKindLLM)
	out = collectSimpleEndpoints(ctx, db, out,
		`SELECT path, COALESCE(requires_auth,0) FROM of_upload_endpoints ORDER BY path`, "POST", authKindUpload)

	return out
}

// collectSimpleEndpoints appends the (path, requires_auth) rows a query returns,
// tagging each with methods and authKind. For the endpoint kinds whose snapshot
// row is just a path plus an auth flag.
func collectSimpleEndpoints(ctx context.Context, db *sql.DB, out []EndpointRef, query, methods, authKind string) []EndpointRef {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var auth bool
		if err := rows.Scan(&p, &auth); err == nil {
			out = append(out, EndpointRef{Path: p, Methods: methods, Auth: auth, AuthKind: authKind})
		}
	}
	_ = rows.Err()
	return out
}

func cleanMethods(m string) string {
	m = strings.ReplaceAll(m, `"`, "")
	m = strings.Trim(m, "[] ")
	m = strings.ReplaceAll(m, " ", "")
	if m == "" {
		return "GET"
	}
	return m
}
