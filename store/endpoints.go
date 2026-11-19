package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"openfabrica/db"
)

var (
	ErrEndpointNotFound  = errors.New("endpoint not found")
	ErrEndpointDuplicate = errors.New("endpoint path already exists")
)

// ProjectEndpoint represents any project API/LLM/WS/stream/upload/function/agent/etc. endpoint.
type ProjectEndpoint struct {
	Kind             string    `json:"kind"`
	Detail           string    `json:"detail,omitempty"`
	ID               int       `json:"id"`
	ProjectID        int       `json:"project_id"`
	Path             string    `json:"path"`
	TableName        string    `json:"table_name"`
	Methods          string    `json:"methods"`
	PublicColumns    *string   `json:"public_columns"`
	RequiresAuth     bool      `json:"requires_auth"`
	PublicRead       bool      `json:"public_read"`
	RequiredRole     *string   `json:"required_role"`
	RateLimit        int       `json:"rate_limit"`
	CreatedAt        time.Time `json:"created_at"`
	DailyTokenBudget int       `json:"daily_token_budget,omitempty"`
	DailyTokensUsed  int       `json:"daily_tokens_used,omitempty"`
	DayWindowStart   string    `json:"day_window_start,omitempty"`
}

// EndpointUpdate describes partial updates to an of_api_endpoints row.
type EndpointUpdate struct {
	MethodsJSON  *string
	RequiresAuth *bool
	PublicRead   *bool
	RequiredRole *string // empty string clears to NULL
	RateLimit    *int
}

type readOnlyEndpointKind struct {
	kind     string
	label    string
	methods  string
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

// ListAllProjectEndpoints returns all API and read-only endpoints for a project.
func ListAllProjectEndpoints(pdb *db.ProjectDB, projectID int) ([]ProjectEndpoint, error) {
	endpoints := []ProjectEndpoint{}

	rows, err := pdb.Query(
		`SELECT id, path, table_name, methods, public_columns, requires_auth, public_read, required_role, rate_limit, created_at
		 FROM of_api_endpoints ORDER BY path ASC`,
	)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e ProjectEndpoint
		e.ProjectID = projectID
		e.Kind = "api"
		var publicCols, reqRole sql.NullString
		if rows.Scan(&e.ID, &e.Path, &e.TableName, &e.Methods, &publicCols, &e.RequiresAuth, &e.PublicRead, &reqRole, &e.RateLimit, &e.CreatedAt) == nil {
			if publicCols.Valid {
				e.PublicColumns = &publicCols.String
			}
			if reqRole.Valid {
				e.RequiredRole = &reqRole.String
			}
			endpoints = append(endpoints, e)
		}
	}
	_ = rows.Err()
	rows.Close()

	llmRows, err := pdb.Query(
		`SELECT id, path, system_prompt, requires_auth, rate_limit, daily_token_budget, daily_tokens_used, day_window_start, created_at
		 FROM of_llm_endpoints ORDER BY path ASC`,
	)
	if err != nil {
		return nil, err
	}
	for llmRows.Next() {
		var e ProjectEndpoint
		e.ProjectID = projectID
		var systemPrompt string
		if llmRows.Scan(&e.ID, &e.Path, &systemPrompt, &e.RequiresAuth, &e.RateLimit, &e.DailyTokenBudget, &e.DailyTokensUsed, &e.DayWindowStart, &e.CreatedAt) == nil {
			e.Methods = `["POST"]`
			e.Kind = "llm"
			e.TableName = "LLM"
			e.PublicRead = !e.RequiresAuth
			e.ID += 100000
			endpoints = append(endpoints, e)
		}
	}
	_ = llmRows.Err()
	llmRows.Close()

	wsRows, err := pdb.Query(
		`SELECT id, path, write_to_table, room_column, requires_auth, created_at
		 FROM of_ws_endpoints ORDER BY path ASC`,
	)
	if err != nil {
		return nil, err
	}
	for wsRows.Next() {
		var e ProjectEndpoint
		e.ProjectID = projectID
		var writeToTable, roomCol string
		if wsRows.Scan(&e.ID, &e.Path, &writeToTable, &roomCol, &e.RequiresAuth, &e.CreatedAt) == nil {
			e.Methods = `["WS"]`
			e.Kind = "ws"
			e.TableName = "WebSocket"
			if writeToTable != "" {
				e.TableName = "WebSocket -> " + writeToTable
			}
			e.PublicRead = !e.RequiresAuth
			e.ID += 200000
			endpoints = append(endpoints, e)
		}
	}
	_ = wsRows.Err()
	wsRows.Close()

	streamRows, err := pdb.Query(
		`SELECT id, path, requires_auth, created_at
		 FROM of_stream_endpoints ORDER BY path ASC`,
	)
	if err != nil {
		return nil, err
	}
	for streamRows.Next() {
		var e ProjectEndpoint
		e.ProjectID = projectID
		if streamRows.Scan(&e.ID, &e.Path, &e.RequiresAuth, &e.CreatedAt) == nil {
			e.Methods = `["SSE"]`
			e.Kind = "stream"
			e.TableName = "Stream"
			e.PublicRead = !e.RequiresAuth
			e.ID += 300000
			endpoints = append(endpoints, e)
		}
	}
	_ = streamRows.Err()
	streamRows.Close()

	uploadRows, err := pdb.Query(
		`SELECT id, path, table_name, max_size_mb, requires_auth, created_at
		 FROM of_upload_endpoints ORDER BY path ASC`,
	)
	if err != nil {
		return nil, err
	}
	for uploadRows.Next() {
		var e ProjectEndpoint
		e.ProjectID = projectID
		var tblName sql.NullString
		var maxSize int
		if uploadRows.Scan(&e.ID, &e.Path, &tblName, &maxSize, &e.RequiresAuth, &e.CreatedAt) == nil {
			e.Methods = `["POST"]`
			e.Kind = "upload"
			e.TableName = "Upload"
			if tblName.Valid && tblName.String != "" {
				e.TableName = "Upload -> " + tblName.String
			}
			e.PublicRead = !e.RequiresAuth
			e.ID += 400000
			endpoints = append(endpoints, e)
		}
	}
	_ = uploadRows.Err()
	uploadRows.Close()

	for _, k := range readOnlyEndpointKinds {
		rRows, err := pdb.Query(k.query)
		if err != nil {
			return nil, err
		}
		for rRows.Next() {
			e := ProjectEndpoint{ProjectID: projectID, Kind: k.kind, TableName: k.label}
			var methods, detail string
			if rRows.Scan(&e.ID, &e.Path, &detail, &methods, &e.RequiresAuth, &e.RateLimit, &e.CreatedAt) == nil {
				e.Detail = detail
				e.PublicRead = !e.RequiresAuth
				e.ID += k.idOffset
				if k.methods != "" {
					methods = k.methods
				}
				e.Methods = methodsJSON(methods)
				endpoints = append(endpoints, e)
			}
		}
		_ = rRows.Err()
		rRows.Close()
	}

	return endpoints, nil
}

// GetAPIEndpoint loads a single of_api_endpoints row by ID.
func GetAPIEndpoint(pdb *db.ProjectDB, projectID, endpointID int) (*ProjectEndpoint, error) {
	var e ProjectEndpoint
	e.ProjectID = projectID
	var publicCols, reqRole sql.NullString
	err := pdb.QueryRow(
		`SELECT id, path, table_name, methods, public_columns, requires_auth, public_read, required_role, rate_limit, created_at
		 FROM of_api_endpoints WHERE id = ?`, endpointID,
	).Scan(&e.ID, &e.Path, &e.TableName, &e.Methods, &publicCols, &e.RequiresAuth, &e.PublicRead, &reqRole, &e.RateLimit, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEndpointNotFound
	}
	if err != nil {
		return nil, err
	}
	if publicCols.Valid {
		e.PublicColumns = &publicCols.String
	}
	if reqRole.Valid {
		e.RequiredRole = &reqRole.String
	}
	return &e, nil
}

// DeleteAPIEndpoint deletes an of_api_endpoints row by ID.
func DeleteAPIEndpoint(pdb *db.ProjectDB, endpointID int) error {
	res, err := pdb.ExecWrite("DELETE FROM of_api_endpoints WHERE id = ?", endpointID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEndpointNotFound
	}
	return nil
}

// UpdateAPIEndpoint applies partial updates to an of_api_endpoints row.
func UpdateAPIEndpoint(pdb *db.ProjectDB, endpointID int, u EndpointUpdate) error {
	var sets []string
	var args []any
	if u.MethodsJSON != nil {
		sets = append(sets, "methods = ?")
		args = append(args, *u.MethodsJSON)
	}
	if u.RequiresAuth != nil {
		sets = append(sets, "requires_auth = ?")
		args = append(args, *u.RequiresAuth)
	}
	if u.PublicRead != nil {
		sets = append(sets, "public_read = ?")
		args = append(args, *u.PublicRead)
	}
	if u.RequiredRole != nil {
		sets = append(sets, "required_role = ?")
		if *u.RequiredRole == "" {
			args = append(args, nil)
		} else {
			args = append(args, *u.RequiredRole)
		}
	}
	if u.RateLimit != nil {
		sets = append(sets, "rate_limit = ?")
		args = append(args, *u.RateLimit)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, endpointID)
	res, err := pdb.ExecWrite(
		fmt.Sprintf("UPDATE of_api_endpoints SET %s WHERE id = ?", strings.Join(sets, ", ")),
		args...,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrEndpointNotFound
	}
	return nil
}

// CreateAPIEndpoint inserts a new of_api_endpoints row after checking table existence and path uniqueness.
func CreateAPIEndpoint(pdb *db.ProjectDB, path, tableName, methodsJSON string, requiresAuth, publicRead bool, requiredRole, ownerColumn string, rateLimit int) error {
	if !DynamicTableExists(pdb, tableName) {
		return ErrTableNotFound
	}
	var existing int
	_ = pdb.QueryRow("SELECT COUNT(*) FROM of_api_endpoints WHERE path = ?", path).Scan(&existing)
	if existing > 0 {
		return ErrEndpointDuplicate
	}
	var rolePtr, ownerPtr *string
	if requiredRole != "" {
		rolePtr = &requiredRole
	}
	if ownerColumn != "" {
		ownerPtr = &ownerColumn
	}
	_, err := pdb.ExecWrite(
		`INSERT INTO of_api_endpoints (path, table_name, methods, requires_auth, public_read, required_role, owner_column, rate_limit)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		path, tableName, methodsJSON, requiresAuth, publicRead, rolePtr, ownerPtr, rateLimit,
	)
	return err
}
