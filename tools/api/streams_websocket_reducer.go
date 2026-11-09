package api

// set_reducer for authoritative realtime endpoints: validate a model-authored
// room reducer with the SAME goja compile the room actor uses at runtime, then
// store it on the endpoint. Split out of streams_websocket.go.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/events"
	"openfabrica/internal/roomactor"
	"openfabrica/internal/wsendpoint"
	"openfabrica/probe"
	"openfabrica/store"
	"openfabrica/tools"
)

// epSetWebSocketReducer validates a model-authored room reducer with the SAME
// goja Compile the actor uses at runtime ("valid on save" == "valid at runtime")
// and stores it on the endpoint, flipping it authoritative. The reducer is never
// served to a browser; the room actor loads it server-side.
func epSetWebSocketReducer(ctx *tools.ToolContext, rawPath, code, seedTable, seedKeyColumn string, seedColumns []string, sharedTable string, sharedColumns []string) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(rawPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	if strings.TrimSpace(code) == "" {
		return tools.Fail("code is required"), nil
	}
	row, err := wsendpoint.Load(ctx.DB, path)
	if err == sql.ErrNoRows {
		return tools.Fail("websocket endpoint %q not found - create it first with manage_authoritative create", path), nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading websocket endpoint: %w", err)
	}
	// Only a manage_authoritative endpoint takes a reducer: attaching one to a relay,
	// tick, or signaling endpoint silently converted it into a different model. (A
	// legacy row with no stored model is let through; matchmaking is refused below.)
	if row.Model != "" {
		if res := checkModelOwnership(row, wsendpoint.ModelAuthoritative); res != nil {
			return res, nil
		}
	}
	// Validate the optional join-seed spec up front: the actor reads these
	// identifiers into a SELECT at join time, so an invalid one must fail here at
	// author time, not silently at runtime. seed_key_column defaults to "id".
	var seedColsJSON string
	if strings.TrimSpace(seedTable) != "" {
		if _, err := store.SanitizedTableName(seedTable); err != nil {
			return tools.Fail("%s", "seed_table rejected: "+err.Error()), nil
		}
		if len(seedColumns) == 0 {
			return tools.Fail("seed_columns is required when seed_table is set"), nil
		}
		if seedKeyColumn == "" {
			seedKeyColumn = "id"
		}
		if !store.ValidColumnName.MatchString(seedKeyColumn) {
			return tools.Fail("%s", "seed_key_column is not a valid column name: "+seedKeyColumn), nil
		}
		for _, c := range seedColumns {
			if !store.ValidColumnName.MatchString(c) {
				return tools.Fail("%s", "seed_columns contains an invalid column name: "+c), nil
			}
		}
		// The seed is keyed on the joining user's authenticated id, which exists only on
		// a requires_auth endpoint: anywhere else it never fires and every player joins
		// with no seed, silently.
		if !row.RequiresAuth {
			return tools.Fail("seed_table needs requires_auth=true: the seed row is looked up by the joining user's authenticated id, which an anonymous endpoint does not have (turn it on with manage_authoritative update requires_auth=true, or drop the seed and key state on event.player.id)"), nil
		}
		if res := checkTableColumns(ctx, "seed_table", seedTable, append([]string{seedKeyColumn}, seedColumns...)); res != nil {
			return res, nil
		}
		b, _ := json.Marshal(seedColumns)
		seedColsJSON = string(b)
	} else {
		// No seed table -> clear any prior seed spec so it can't linger.
		seedKeyColumn = ""
	}
	// Validate the optional shared/global read spec (injected as event.shared each
	// non-@tick event). Same author-time validation as the seed spec. Empty
	// shared_columns means all NON-SECURE columns (readShared excludes PASSWORD/ENCRYPTED).
	var sharedColsJSON string
	if strings.TrimSpace(sharedTable) != "" {
		if _, err := store.SanitizedTableName(sharedTable); err != nil {
			return tools.Fail("%s", "shared_table rejected: "+err.Error()), nil
		}
		for _, c := range sharedColumns {
			if !store.ValidColumnName.MatchString(c) {
				return tools.Fail("%s", "shared_columns contains an invalid column name: "+c), nil
			}
		}
		if res := checkTableColumns(ctx, "shared_table", sharedTable, sharedColumns); res != nil {
			return res, nil
		}
		if len(sharedColumns) > 0 {
			b, _ := json.Marshal(sharedColumns)
			sharedColsJSON = string(b)
		}
	}
	// An authoritative reducer and matchmaking cannot share one endpoint: the room
	// actor binds to the connect room and disables the in-band room switch (stream.go)
	// that matched peers rely on. Refuse rather than ship a silently-broken combo
	// (mirrors the matchmaking+room_gated refusal in epCreateWebSocket).
	if row.Matchmaking {
		return tools.Fail("authoritative reducer is incompatible with matchmaking (matched peers switch rooms in-band, which the room actor disables). Run the authoritative game on a separate endpoint and route matched peers to it via ?room=."), nil
	}
	r, err := roomactor.Compile(code)
	if err != nil {
		return tools.Fail("%s", "reducer rejected (would not load): "+err.Error()), nil
	}
	// Run it through the same lifecycle the build validator drives (@init, two @join,
	// @tick/@timer, view) so a throw is reported now, against the code just written,
	// instead of at build end. A reducer that fails here would fail the build anyway.
	if issues := probe.CheckReducerSource(path, code, row.TickHz); len(issues) > 0 {
		msgs := make([]string, 0, len(issues))
		for _, iss := range issues {
			msgs = append(msgs, iss.Message)
		}
		return tools.Fail("reducer rejected (NOT attached) - it fails the room lifecycle run:\n- %s\nFix the code and call set_reducer again.", strings.Join(msgs, "\n- ")), nil
	}
	res, err := ctx.DB.Exec("UPDATE of_ws_endpoints SET reducer = ?, authoritative = 1, seed_table = ?, seed_key_column = ?, seed_columns = ?, shared_table = ?, shared_columns = ? WHERE path = ?", code, seedTable, seedKeyColumn, seedColsJSON, sharedTable, sharedColsJSON, path)
	if err != nil {
		return nil, fmt.Errorf("storing reducer: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tools.Fail("websocket endpoint %q not found - create it first", path), nil
	}
	// Hand rooms that are live right now the new code, rather than waiting for the next
	// join (a re-test of an open room would otherwise keep showing the old behaviour).
	if ctx.Bus != nil {
		ctx.Bus.Publish(events.NewEvent(events.EventWSReducerChanged, ctx.ProjectID, map[string]any{"endpoint_path": path}))
	}
	out := map[string]any{
		"endpoint_path": path,
		"authoritative": true,
		"has_view":      r.HasView(),
	}
	// set_reducer writes the whole spec, so a re-attach that leaves the seed/shared
	// tables out clears them. Say so, or a fixed reducer silently loses its seed.
	var cleared []string
	if row.SeedTable != "" && strings.TrimSpace(seedTable) == "" {
		cleared = append(cleared, "seed_table "+row.SeedTable)
	}
	if row.SharedTable != "" && strings.TrimSpace(sharedTable) == "" {
		cleared = append(cleared, "shared_table "+row.SharedTable)
	}
	var warnings []string
	if len(cleared) > 0 {
		warnings = append(warnings, "this call cleared the previously attached "+strings.Join(cleared, " and ")+"; pass them again with the reducer to keep them")
	}
	if w := probe.ReducerTickWriteWarning(path, code, row.TickHz); w != "" {
		warnings = append(warnings, w)
	}
	if len(warnings) > 0 {
		out["warning"] = strings.Join(warnings, " | ")
	}
	out["note"] = "Reducer attached and validated. The room actor applies _action frames via reduce() and pushes each player {_type:'state', state, rev} (their view()). " +
		"Clients (net = the connect() handle from 'ofnet'): net.action(type, payload) to act, net.onState(cb) to render. Do NOT also relay game state by hand - the actor is the source of truth."
	return tools.Ok(out), nil
}

// checkTableColumns verifies a table set_reducer will read from exists and has the named
// columns, so a typo fails at attach time instead of shipping a seed/shared read that
// silently returns nothing.
func checkTableColumns(ctx *tools.ToolContext, arg, table string, cols []string) *tools.Result {
	have, exists := dynamicTableColumns(ctx, table)
	if !exists {
		return tools.Fail("%s %q does not exist - create it first (manage_schema create_table)", arg, table)
	}
	if have == nil {
		return nil
	}
	for _, c := range cols {
		if c != "id" && !have[c] {
			return tools.Fail("%s %q has no column %q", arg, table, c)
		}
	}
	return nil
}
