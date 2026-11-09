package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/internal/endpoints"
	"openfabrica/tools"
)

// SSE (server-sent events) transport handlers for manage_sse. Tool
// registration and arg shape live in manage_sse.go.

// SSE stream endpoint actions

// sseBasePath reads an SSE endpoint path given by its route ("orders/stream") as the
// resource it streams ("orders"). The plan names an SSE task's deliverable by that
// route, so the model passes it on (some builds); the runtime appends /stream
// itself and the suffix can mean nothing else here, so it is read off, not rejected.
func sseBasePath(raw string) string {
	p := tools.NormalizeEndpointPath(raw)
	if base, ok := strings.CutSuffix(p, "/stream"); ok && base != "" {
		return base
	}
	return p
}

// epCreateStream takes typed args straight from SSECreate.
func epCreateStream(ctx *tools.ToolContext, rawPath string, requiresAuth bool, tableName string, eventTypesRaw []any) (*tools.Result, error) {
	path, errRes := tools.ValidateAndNormalizeEndpointPath(sseBasePath(rawPath))
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}
	if msg := sseTableError(ctx, tableName); msg != "" {
		return tools.FailCode(tools.CodeBadArg, "%s", msg), nil
	}

	// Skip if stream endpoint already exists (prevents duplicate creation during BUILD).
	var existingCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_stream_endpoints WHERE path = ?", path).Scan(&existingCount); err == nil && existingCount > 0 {
		return tools.Ok(map[string]any{
			"endpoint_path":   path,
			"already_existed": true,
			"message":         "Stream endpoint /api/" + path + " already exists, so nothing was created and NO option in this call was applied. Change its config with manage_sse action=update.",
		}), nil
	}

	// Parse event_types (default: all data events).
	eventTypes := []string{"data.insert", "data.update", "data.delete"}
	if len(eventTypesRaw) > 0 {
		eventTypes = nil
		for _, et := range eventTypesRaw {
			if ets, ok := et.(string); ok {
				eventTypes = append(eventTypes, ets)
			}
		}
	}
	eventTypesJSON, _ := json.Marshal(eventTypes)

	if err := ClaimEndpoint(ctx, endpoints.KindSSE, path, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(
		`INSERT INTO of_stream_endpoints (path, event_types, table_name, requires_auth)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   event_types = excluded.event_types,
		   table_name = excluded.table_name,
		   requires_auth = excluded.requires_auth`,
		path, string(eventTypesJSON), tableName, requiresAuth,
	)
	if err != nil {
		return nil, fmt.Errorf("creating stream endpoint: %w", err)
	}

	return tools.Ok(map[string]any{
		"endpoint_path": path,
		"route":         fmt.Sprintf("GET /api/%s/stream", path),
		"event_types":   eventTypes,
		"table_name":    tableName,
		"requires_auth": requiresAuth,
		"usage":         sseUsage(path, requiresAuth),
	}), nil
}

// sseUsage is the client snippet returned on create. On a requires_auth endpoint
// it carries the token as ?token=: EventSource cannot set an Authorization header,
// so the tokenless form is refused 401 before the stream opens (the server accepts
// the query form for exactly this reason - see server/public/stream.go).
func sseUsage(path string, requiresAuth bool) string {
	open := "const source = new EventSource('/api/" + path + "/stream');"
	if requiresAuth {
		open = "const source = new EventSource('/api/" + path + "/stream?token=' + encodeURIComponent(localStorage.getItem('auth_token')));" +
			" // requires_auth: EventSource cannot send an Authorization header, so the token rides the query string; gate the page with requireAuth (imported from 'auth') so it exists first."
	}
	return open + " source.addEventListener('data.insert', (e) => { const data = JSON.parse(e.data); ... });"
}

// epUpdateStream takes typed args straight from SSEUpdate. Each field is
// patched only when the caller actually passed it. requires_auth and
// table_name are tri-state pointers: a plain bool/string merge could never
// express `requires_auth: false` (disable the gate) or `table_name: ""`
// (widen back to all tables) - the zero value read as "unset".
func epUpdateStream(ctx *tools.ToolContext, rawPath string, requiresAuth *bool, tableName *string, eventTypesRaw []any) (*tools.Result, error) {
	path := sseBasePath(rawPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	setClauses := []string{}
	var vals []any

	if len(eventTypesRaw) > 0 {
		var types []string
		for _, et := range eventTypesRaw {
			if s, ok := et.(string); ok {
				types = append(types, s)
			}
		}
		b, _ := json.Marshal(types)
		setClauses = append(setClauses, "event_types = ?")
		vals = append(vals, string(b))
	}
	if requiresAuth != nil {
		setClauses = append(setClauses, "requires_auth = ?")
		vals = append(vals, *requiresAuth)
	}
	if tableName != nil {
		if msg := sseTableError(ctx, *tableName); msg != "" {
			return tools.FailCode(tools.CodeBadArg, "%s", msg), nil
		}
		setClauses = append(setClauses, "table_name = ?")
		vals = append(vals, *tableName)
	}

	if len(setClauses) == 0 {
		return tools.Fail("no fields to update"), nil
	}

	vals = append(vals, path)
	query := fmt.Sprintf("UPDATE of_stream_endpoints SET %s WHERE path = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("updating stream endpoint: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.Fail("stream endpoint %q not found", path), nil
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "updated": true}), nil
}

func epListStream(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT id, path, event_types, COALESCE(table_name,''), requires_auth, created_at FROM of_stream_endpoints ORDER BY path")
	if err != nil {
		return nil, fmt.Errorf("listing stream endpoints: %w", err)
	}
	defer rows.Close()
	var endpoints []map[string]any
	for rows.Next() {
		var id int
		var path, eventTypes, tableName string
		var requiresAuth bool
		var createdAt time.Time
		if rows.Scan(&id, &path, &eventTypes, &tableName, &requiresAuth, &createdAt) != nil {
			continue
		}
		endpoints = append(endpoints, map[string]any{
			"id": id, "endpoint_path": path, "event_types": eventTypes,
			"table_name": tableName, "requires_auth": requiresAuth, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing stream endpoints: %w", err)
	}
	return tools.Ok(endpoints), nil
}

func epDeleteStream(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_stream_endpoints", "stream endpoint", "manage_sse", "delete", sseBasePath(endpointPath))
}

// sseTableError checks a stream's table_name scope against the project's tables, so a
// typo or an endpoint route passed instead of the table name fails at build time
// rather than leaving a stream that never emits. Empty table_name (all tables) passes.
func sseTableError(ctx *tools.ToolContext, tableName string) string {
	if strings.TrimSpace(tableName) == "" {
		return ""
	}
	var n int
	_ = ctx.DB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&n)
	if n > 0 {
		return ""
	}
	return fmt.Sprintf("table_name %q is not a project table - pass the table name you gave create_table (not the endpoint route), or omit it to stream every table's events", tableName)
}
