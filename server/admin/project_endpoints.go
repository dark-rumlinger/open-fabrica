package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// ProjectEndpointsHandler handles API endpoint browsing for project detail views.
type ProjectEndpointsHandler struct {
	deps *Deps
}

type projectEndpoint struct {
	// Kind names what sort of endpoint the row is: "api" (a table's REST API, the
	// only kind this page can edit or delete) or one of the read-only kinds listed
	// below. The id of a non-api row is offset so it cannot collide with an api id.
	Kind string `json:"kind"`
	// Detail is one line of what the endpoint does, for the kinds that have no
	// table to show (a function's timeout, an agent's step limit, a feed's format).
	Detail        string    `json:"detail,omitempty"`
	ID            int       `json:"id"`
	ProjectID     int       `json:"project_id"`
	Path          string    `json:"path"`
	TableName     string    `json:"table_name"`
	Methods       string    `json:"methods"`
	PublicColumns *string   `json:"public_columns"`
	RequiresAuth  bool      `json:"requires_auth"`
	PublicRead    bool      `json:"public_read"`
	RequiredRole  *string   `json:"required_role"`
	RateLimit     int       `json:"rate_limit"`
	CreatedAt     time.Time `json:"created_at"`

	// LLM-proxy-only fields. Omitted (zero) for non-LLM rows so the existing
	// UI keeps rendering API endpoints unchanged; the LLM row carries the
	// extra columns admins need to spot a runaway endpoint at a glance.
	DailyTokenBudget int    `json:"daily_token_budget,omitempty"`
	DailyTokensUsed  int    `json:"daily_tokens_used,omitempty"`
	DayWindowStart   string `json:"day_window_start,omitempty"`
}

type updateEndpointRequest struct {
	Methods      []string `json:"methods"`
	RequiresAuth *bool    `json:"requires_auth"`
	PublicRead   *bool    `json:"public_read"`
	RequiredRole *string  `json:"required_role"`
	RateLimit    *int     `json:"rate_limit"`
}

// List returns all API endpoints for a project.
func (h *ProjectEndpointsHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	rows, err := projectDB.Query(
		`SELECT id, path, table_name, methods, public_columns, requires_auth, public_read, required_role, rate_limit, created_at
		 FROM of_api_endpoints ORDER BY path ASC`,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []projectEndpoint{})
		return
	}

	var endpoints []projectEndpoint
	endpoints = append(endpoints, collectRows(rows, func(rows *sql.Rows) (projectEndpoint, error) {
		var e projectEndpoint
		e.ProjectID = projectID
		var publicCols, reqRole sql.NullString
		if err := rows.Scan(&e.ID, &e.Path, &e.TableName, &e.Methods, &publicCols, &e.RequiresAuth, &e.PublicRead, &reqRole, &e.RateLimit, &e.CreatedAt); err != nil {
			return e, err
		}
		if publicCols.Valid {
			e.PublicColumns = &publicCols.String
		}
		if reqRole.Valid {
			e.RequiredRole = &reqRole.String
		}
		return e, nil
	})...)

	// Also include LLM endpoints so they appear in the admin panel.
	if llmRows, err := projectDB.Query(
		`SELECT id, path, system_prompt, requires_auth, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, created_at
		 FROM of_llm_endpoints ORDER BY path ASC`,
	); err == nil {
		endpoints = append(endpoints, collectRows(llmRows, func(rows *sql.Rows) (projectEndpoint, error) {
			var e projectEndpoint
			e.ProjectID = projectID
			var systemPrompt string
			if err := rows.Scan(&e.ID, &e.Path, &systemPrompt, &e.RequiresAuth, &e.RateLimit, &e.DailyTokenBudget, &e.DailyTokensUsed, &e.DayWindowStart, &e.CreatedAt); err != nil {
				return e, err
			}
			e.Methods = `["POST"]`
			e.Kind = "llm"
			e.TableName = "LLM"
			e.PublicRead = !e.RequiresAuth
			// Offset IDs to avoid collision with API endpoint IDs.
			e.ID = e.ID + 100000
			return e, nil
		})...)
	}

	// Include WebSocket endpoints.
	if wsRows, err := projectDB.Query(
		`SELECT id, path, write_to_table, room_column, requires_auth, created_at
		 FROM of_ws_endpoints ORDER BY path ASC`,
	); err == nil {
		endpoints = append(endpoints, collectRows(wsRows, func(rows *sql.Rows) (projectEndpoint, error) {
			var e projectEndpoint
			e.ProjectID = projectID
			var writeToTable, roomCol string
			if err := rows.Scan(&e.ID, &e.Path, &writeToTable, &roomCol, &e.RequiresAuth, &e.CreatedAt); err != nil {
				return e, err
			}
			e.Methods = `["WS"]`
			e.Kind = "ws"
			e.TableName = "WebSocket"
			if writeToTable != "" {
				e.TableName = "WebSocket -> " + writeToTable
			}
			e.PublicRead = !e.RequiresAuth
			e.ID = e.ID + 200000
			return e, nil
		})...)
	}

	// Include SSE stream endpoints.
	if streamRows, err := projectDB.Query(
		`SELECT id, path, requires_auth, created_at
		 FROM of_stream_endpoints ORDER BY path ASC`,
	); err == nil {
		endpoints = append(endpoints, collectRows(streamRows, func(rows *sql.Rows) (projectEndpoint, error) {
			var e projectEndpoint
			e.ProjectID = projectID
			if err := rows.Scan(&e.ID, &e.Path, &e.RequiresAuth, &e.CreatedAt); err != nil {
				return e, err
			}
			e.Methods = `["SSE"]`
			e.Kind = "stream"
			e.TableName = "Stream"
			e.PublicRead = !e.RequiresAuth
			e.ID = e.ID + 300000
			return e, nil
		})...)
	}

	// Include upload endpoints.
	if uploadRows, err := projectDB.Query(
		`SELECT id, path, table_name, max_size_mb, requires_auth, created_at
		 FROM of_upload_endpoints ORDER BY path ASC`,
	); err == nil {
		endpoints = append(endpoints, collectRows(uploadRows, func(rows *sql.Rows) (projectEndpoint, error) {
			var e projectEndpoint
			e.ProjectID = projectID
			var tblName sql.NullString
			var maxSize int
			if err := rows.Scan(&e.ID, &e.Path, &tblName, &maxSize, &e.RequiresAuth, &e.CreatedAt); err != nil {
				return e, err
			}
			e.Methods = `["POST"]`
			e.Kind = "upload"
			e.TableName = "Upload"
			if tblName.Valid && tblName.String != "" {
				e.TableName = "Upload -> " + tblName.String
			}
			e.PublicRead = !e.RequiresAuth
			e.ID = e.ID + 400000
			return e, nil
		})...)
	}

	// The remaining kinds, each in its own table, shown read-only: the builder
	// creates and changes them with its tools, and nothing on this page can edit
	// them, so they carry no actions. A table that is missing (an older database)
	// simply contributes nothing.
	for _, k := range readOnlyEndpointKinds {
		rows, err := projectDB.Query(k.query)
		if err != nil {
			continue
		}
		endpoints = append(endpoints, collectRows(rows, func(rows *sql.Rows) (projectEndpoint, error) {
			e := projectEndpoint{ProjectID: projectID, Kind: k.kind, TableName: k.label}
			var methods, detail string
			if err := rows.Scan(&e.ID, &e.Path, &detail, &methods, &e.RequiresAuth, &e.RateLimit, &e.CreatedAt); err != nil {
				return e, err
			}
			e.Detail = detail
			e.PublicRead = !e.RequiresAuth
			e.ID += k.idOffset
			if k.methods != "" {
				methods = k.methods
			}
			e.Methods = methodsJSON(methods)
			return e, nil
		})...)
	}

	for i := range endpoints {
		if endpoints[i].Kind == "" {
			endpoints[i].Kind = "api"
		}
	}
	if endpoints == nil {
		endpoints = []projectEndpoint{}
	}

	writeJSON(w, http.StatusOK, endpoints)
}

// readOnlyEndpointKind describes how one endpoint table is listed. query selects,
// in order: id, path, a one-line detail, the methods (ignored when methods is set),
// whether auth is required, the rate limit (0 when the kind has none) and the
// creation time.
type readOnlyEndpointKind struct {
	kind     string
	label    string
	methods  string // fixed methods, comma separated; empty = read them from the query
	idOffset int
	query    string
}

var readOnlyEndpointKinds = []readOnlyEndpointKind{
	{"function", "Function", "", 500000,
		`SELECT id, path, 'timeout ' || CASE WHEN timeout_ms > 0 THEN (timeout_ms || ' ms') ELSE 'default' END, COALESCE(methods,''), COALESCE(requires_auth,0), rate_limit, created_at
		 FROM of_function_endpoints ORDER BY path`},
	{"agent", "Agent", "POST", 600000,
		`SELECT id, path, 'up to ' || max_steps || ' steps', '', COALESCE(requires_auth,1), COALESCE(rate_limit,10), created_at
		 FROM of_agent_endpoints ORDER BY path`},
	{"document", "Document (PDF)", "POST", 700000,
		`SELECT id, path, page_size || CASE WHEN landscape THEN ' landscape' ELSE '' END, '', COALESCE(requires_auth,1), COALESCE(rate_limit,10), created_at
		 FROM of_document_endpoints ORDER BY path`},
	{"export", "Export", "GET", 800000,
		`SELECT id, path, upper(format) || ' of ' || table_name, '', COALESCE(requires_auth,0), COALESCE(rate_limit,30), created_at
		 FROM of_export_endpoints ORDER BY path`},
	{"import", "Import", "POST", 900000,
		`SELECT id, path, mode || ' into ' || table_name, '', COALESCE(requires_auth,1), 0, created_at
		 FROM of_import_endpoints ORDER BY path`},
	{"query", "SQL query", "POST", 1000000,
		`SELECT id, path, 'read-only, up to ' || max_rows || ' rows', '', COALESCE(requires_auth,0), COALESCE(rate_limit,60), created_at
		 FROM of_query_endpoints ORDER BY path`},
	{"feed", "Feed", "GET", 1100000,
		`SELECT id, path, upper(format) || ' of ' || table_name, '', 0, 0, created_at
		 FROM of_feed_endpoints ORDER BY path`},
	{"push", "Push notifications", "POST", 1200000,
		`SELECT id, path, 'web push subscriptions', '', COALESCE(requires_auth,0), 0, created_at
		 FROM of_push_endpoints ORDER BY path`},
	{"totp", "Two-factor codes", "POST", 1300000,
		`SELECT id, path, 'on login API ' || auth_endpoint_path || CASE WHEN enforced THEN ' (required)' ELSE ' (optional)' END, '', 1, 0, created_at
		 FROM of_totp_endpoints ORDER BY path`},
	{"magic_link", "Email sign-in link", "GET,POST", 1400000,
		`SELECT id, path, 'on login API ' || auth_endpoint_path || ', link lasts ' || ttl_minutes || ' min', '', 0, 0, created_at
		 FROM of_magic_link_endpoints ORDER BY path`},
}

// methodsJSON renders a comma-separated method list as the JSON array the page
// parses ("GET,POST" -> ["GET","POST"]). An empty list is an empty array.
func methodsJSON(csv string) string {
	out := []string{}
	for _, m := range strings.Split(csv, ",") {
		if m = strings.ToUpper(strings.TrimSpace(m)); m != "" {
			out = append(out, m)
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// Delete removes an API endpoint by ID.
func (h *ProjectEndpointsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}

	res, err := projectDB.ExecWrite(
		"DELETE FROM of_api_endpoints WHERE id = ?",
		endpointID,
	)
	if err != nil {
		h.deps.serverError(w, "failed to delete endpoint", err)
		return
	}
	// Only data APIs are deleted here. The other kinds carry offset ids that match
	// no row, and reporting success for them would tell the owner something was
	// removed when nothing was.
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "endpoint not found (only data APIs can be deleted here)")
		return
	}

	writeStatus(w, "deleted")
}

// Get returns a single API endpoint by ID.
func (h *ProjectEndpointsHandler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}

	var e projectEndpoint
	e.ProjectID = projectID
	var publicCols, reqRole sql.NullString
	err := projectDB.QueryRow(
		`SELECT id, path, table_name, methods, public_columns, requires_auth, public_read, required_role, rate_limit, created_at
		 FROM of_api_endpoints WHERE id = ?`, endpointID,
	).Scan(&e.ID, &e.Path, &e.TableName, &e.Methods, &publicCols, &e.RequiresAuth, &e.PublicRead, &reqRole, &e.RateLimit, &e.CreatedAt)
	if err != nil {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	if publicCols.Valid {
		e.PublicColumns = &publicCols.String
	}
	if reqRole.Valid {
		e.RequiredRole = &reqRole.String
	}

	writeJSON(w, http.StatusOK, e)
}

// Update modifies an existing API endpoint.
func (h *ProjectEndpointsHandler) Update(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	endpointID, ok := requireIntParam(w, r, "endpointID", "invalid endpoint ID")
	if !ok {
		return
	}

	var req updateEndpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var fields []colVal

	if len(req.Methods) > 0 {
		// Validate methods.
		valid := map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true}
		for _, m := range req.Methods {
			if !valid[strings.ToUpper(m)] {
				writeError(w, http.StatusBadRequest, "invalid method: "+m)
				return
			}
		}
		methodsJSON, _ := json.Marshal(req.Methods)
		fields = append(fields, colVal{"methods", string(methodsJSON)})
	}
	if req.RequiresAuth != nil {
		fields = append(fields, colVal{"requires_auth", *req.RequiresAuth})
	}
	if req.PublicRead != nil {
		fields = append(fields, colVal{"public_read", *req.PublicRead})
	}
	if req.RequiredRole != nil {
		if *req.RequiredRole == "" {
			fields = append(fields, colVal{"required_role", nil})
		} else {
			fields = append(fields, colVal{"required_role", *req.RequiredRole})
		}
	}
	if req.RateLimit != nil {
		fields = append(fields, colVal{"rate_limit", *req.RateLimit})
	}

	query, values, ok := buildUpdate("of_api_endpoints", fields, "id", endpointID)
	if !ok {
		writeError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	res, err := projectDB.ExecWrite(query, values...)
	if err != nil {
		h.deps.serverError(w, "failed to update endpoint", err)
		return
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}

	writeStatus(w, "updated")
}

type createEndpointRequest struct {
	Path         string   `json:"path"`
	TableName    string   `json:"table_name"`
	Methods      []string `json:"methods"`
	RequiresAuth bool     `json:"requires_auth"`
	PublicRead   bool     `json:"public_read"`
	RequiredRole string   `json:"required_role"`
	OwnerColumn  string   `json:"owner_column"`
	RateLimit    int      `json:"rate_limit"`
}

// Create adds a new API endpoint for a project.
func (h *ProjectEndpointsHandler) Create(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	var req createEndpointRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Path == "" || req.TableName == "" {
		writeError(w, http.StatusBadRequest, "path and table_name are required")
		return
	}

	// A scope column is meaningless without an authenticated identity to scope
	// to. requires_auth=false + owner_column (and not public_read) would create
	// an endpoint the public dispatcher refuses at request time; reject it here
	// with a clear message instead of producing a dead or leaky endpoint. Use
	// public_read=true if you want public, intentionally unscoped reads.
	if req.OwnerColumn != "" && !req.RequiresAuth && !req.PublicRead {
		writeError(w, http.StatusBadRequest, "owner_column requires requires_auth=true (or set public_read=true for public, unscoped reads)")
		return
	}

	// Verify the table exists.
	var tableCount int
	_ = projectDB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", req.TableName).Scan(&tableCount) // scan failure leaves 0, rejected below
	if tableCount == 0 {
		writeError(w, http.StatusBadRequest, "table '"+req.TableName+"' does not exist")
		return
	}

	// Check for duplicates.
	var existing int
	_ = projectDB.QueryRow("SELECT COUNT(*) FROM of_api_endpoints WHERE path = ?", req.Path).Scan(&existing) // scan failure leaves 0 (no duplicate)
	if existing > 0 {
		writeError(w, http.StatusConflict, "endpoint path '"+req.Path+"' already exists")
		return
	}

	methods := req.Methods
	if len(methods) == 0 {
		methods = []string{"GET", "POST", "PUT", "DELETE"}
	}
	methodsJSON, _ := json.Marshal(methods)

	rateLimit := req.RateLimit
	if rateLimit <= 0 {
		rateLimit = 60
	}

	var requiredRole *string
	if req.RequiredRole != "" {
		requiredRole = &req.RequiredRole
	}
	var ownerColumn *string
	if req.OwnerColumn != "" {
		ownerColumn = &req.OwnerColumn
	}

	_, err := projectDB.ExecWrite(
		`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, public_read, required_role, owner_column, rate_limit)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Path, req.TableName, string(methodsJSON), req.RequiresAuth, req.PublicRead, requiredRole, ownerColumn, rateLimit,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create endpoint")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"path": req.Path, "created": true})
}
