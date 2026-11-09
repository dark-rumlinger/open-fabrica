package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// generate_docs, auto-generate an OpenAPI 3.0 spec served at /api/docs.json

// columnDef represents a column in a dynamic table schema.
type columnDef struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// parseTableSchemaDef adapts the stored schema to what the OpenAPI builder
// documents. store.ParseSchemaDef is the single reader for schema_def's two
// column forms (bare type string / constraint object) and returns them sorted.
//
// Required here means "the request body must carry it", so a column the server can
// fill on its own - a DEFAULT, or a `generate` expression - is not required even
// when it is NOT NULL.
func parseTableSchemaDef(schemaDef string) []columnDef {
	stored := store.ParseSchemaDef(schemaDef)
	cols := make([]columnDef, 0, len(stored))
	for _, c := range stored {
		cols = append(cols, columnDef{
			Name:     c.Name,
			Type:     c.Type,
			Required: c.NotNull && !c.ServerFills,
		})
	}
	return cols
}

// sqlTypeToOpenAPI maps SQLite column types to OpenAPI types.
func sqlTypeToOpenAPI(sqlType string) string {
	switch strings.ToUpper(sqlType) {
	case "INTEGER":
		return "integer"
	case "REAL":
		return "number"
	case "BOOLEAN":
		return "boolean"
	default:
		return "string"
	}
}

// componentName turns an endpoint path into an OpenAPI component name. Component names
// may hold only letters, digits and a few punctuation marks, so a nested path
// ("orders/recent") must not carry its slash into a $ref (an invalid JSON pointer).
func componentName(path string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, titleCase(path))
}

func epGenerateDocs(ctx *tools.ToolContext) (*tools.Result, error) {
	// Get project name for the API title. A context with no global DB (a headless
	// caller) falls back to the generic title.
	var projectName string
	if ctx.GlobalDB != nil {
		if err := ctx.GlobalDB.QueryRow("SELECT name FROM projects WHERE id = ?", ctx.ProjectID).Scan(&projectName); err != nil {
			projectName = ""
		}
	}
	if projectName == "" {
		projectName = "API"
	}
	var err error

	spec := map[string]any{
		"openapi": "3.0.0",
		"info": map[string]any{
			"title":       projectName + " API",
			"version":     "1.0.0",
			"description": "Auto-generated API documentation",
		},
		"paths":      map[string]any{},
		"components": map[string]any{"schemas": map[string]any{}, "securitySchemes": map[string]any{"BearerAuth": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT"}}},
	}

	paths := spec["paths"].(map[string]any)
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)

	// Load all table schemas from of_dynamic_tables.
	tableSchemas := map[string][]columnDef{}
	schemaRows, err := ctx.DB.Query("SELECT table_name, schema_def FROM of_dynamic_tables")
	if err == nil {
		defer schemaRows.Close()
		for schemaRows.Next() {
			var tName, schemaDef string
			if schemaRows.Scan(&tName, &schemaDef) == nil {
				if cols := parseTableSchemaDef(schemaDef); len(cols) > 0 {
					tableSchemas[tName] = cols
				}
			}
		}
		_ = schemaRows.Err() // best-effort docs input; tolerated like the Query error
	}

	// --- API endpoints ---
	apiRows, err := ctx.DB.Query("SELECT path, table_name, methods, requires_auth, public_read, public_columns, COALESCE(public_write,0) FROM of_api_endpoints")
	if err == nil {
		defer apiRows.Close()
		for apiRows.Next() {
			var path, tableName, methodsJSON string
			var requiresAuth, publicRead, publicWrite bool
			var publicCols sql.NullString
			if apiRows.Scan(&path, &tableName, &methodsJSON, &requiresAuth, &publicRead, &publicCols, &publicWrite) != nil {
				continue
			}

			var methods []string
			if json.Unmarshal([]byte(methodsJSON), &methods) != nil {
				methods = []string{"GET", "POST", "PUT", "DELETE"}
			}

			// Build request/response schemas from table columns.
			cols := tableSchemas[tableName]
			reqProps := map[string]any{}
			respProps := map[string]any{"id": map[string]any{"type": "integer"}}
			var requiredFields []string
			for _, c := range cols {
				if strings.ToUpper(c.Type) == "PASSWORD" {
					continue // exclude from response
				}
				prop := map[string]any{"type": sqlTypeToOpenAPI(c.Type)}
				respProps[c.Name] = prop
				reqProps[c.Name] = prop
				if c.Required {
					requiredFields = append(requiredFields, c.Name)
				}
			}
			// Add password columns to request schema only.
			for _, c := range cols {
				if strings.ToUpper(c.Type) == "PASSWORD" {
					reqProps[c.Name] = map[string]any{"type": "string", "format": "password"}
					if c.Required {
						requiredFields = append(requiredFields, c.Name)
					}
				}
			}

			schemaName := componentName(path)
			schemas[schemaName] = map[string]any{
				"type":       "object",
				"properties": respProps,
			}
			reqSchemaName := schemaName + "Input"
			reqSchema := map[string]any{
				"type":       "object",
				"properties": reqProps,
			}
			if len(requiredFields) > 0 {
				reqSchema["required"] = requiredFields
			}
			schemas[reqSchemaName] = reqSchema

			apiPath := "/api/" + path
			apiPathID := "/api/" + path + "/{id}"
			pathItem := map[string]any{}
			pathItemID := map[string]any{}

			for _, m := range methods {
				switch strings.ToUpper(m) {
				case "GET":
					getOp := map[string]any{
						"summary":     "List " + path,
						"operationId": "list_" + path,
						"parameters": []any{
							map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "default": 50}},
							map[string]any{"name": "offset", "in": "query", "schema": map[string]any{"type": "integer", "default": 0}},
							map[string]any{"name": "q", "in": "query", "schema": map[string]any{"type": "string"}, "description": "Full-text search across TEXT columns"},
							map[string]any{"name": "sort", "in": "query", "schema": map[string]any{"type": "string"}, "description": "Column to sort by (pair with order=asc|desc; default id desc)"},
							map[string]any{"name": "order", "in": "query", "schema": map[string]any{"type": "string", "enum": []any{"asc", "desc"}}},
							map[string]any{"name": "<column>", "in": "query", "schema": map[string]any{"type": "string"}, "description": "Per-column filter. <col>=value (exact), <col>__like=term (substring), <col>__has=token (whole token in a comma-separated column), <col>__gt/__gte/__lt/__lte/__ne (comparisons)."},
						},
						"responses": map[string]any{
							"200": jsonResp("Success", map[string]any{
								"type": "object",
								"properties": map[string]any{
									"rows":   map[string]any{"type": "array", "items": schemaRef(schemaName)},
									"count":  map[string]any{"type": "integer"},
									"limit":  map[string]any{"type": "integer"},
									"offset": map[string]any{"type": "integer"},
								},
							}),
						},
					}
					pathItem["get"] = secure(getOp, requiresAuth && !publicRead)

					getOneOp := map[string]any{
						"summary":     "Get " + path + " by ID",
						"operationId": "get_" + path,
						"parameters":  idPathParam(),
						"responses": map[string]any{
							"200": jsonResp("Success", schemaRef(schemaName)),
							"404": map[string]any{"description": "Not found"},
						},
					}
					pathItemID["get"] = secure(getOneOp, requiresAuth && !publicRead)

				case "POST":
					postOp := map[string]any{
						"summary":     "Create " + path,
						"operationId": "create_" + path,
						"requestBody": map[string]any{
							"required": true,
							"content":  jsonContent(schemaRef(reqSchemaName)),
						},
						"responses": map[string]any{
							"201": jsonResp("Created", schemaRef(schemaName)),
						},
					}
					pathItem["post"] = secure(postOp, requiresAuth && !publicWrite)

				case "PUT":
					putOp := map[string]any{
						"summary":     "Update " + path,
						"operationId": "update_" + path,
						"parameters":  idPathParam(),
						"requestBody": map[string]any{
							"required": true,
							"content":  jsonContent(schemaRef(reqSchemaName)),
						},
						"responses": map[string]any{
							"200": jsonResp("Updated", schemaRef(schemaName)),
							"404": map[string]any{"description": "Not found"},
						},
					}
					pathItemID["put"] = secure(putOp, requiresAuth && !publicWrite)

				case "DELETE":
					delOp := map[string]any{
						"summary":     "Delete " + path,
						"operationId": "delete_" + path,
						"parameters":  idPathParam(),
						"responses": map[string]any{
							"200": map[string]any{"description": "Deleted"},
							"404": map[string]any{"description": "Not found"},
						},
					}
					pathItemID["delete"] = secure(delOp, requiresAuth && !publicWrite)
				}
			}

			if len(pathItem) > 0 {
				paths[apiPath] = pathItem
			}
			if len(pathItemID) > 0 {
				paths[apiPathID] = pathItemID
			}
		}
		_ = apiRows.Err() // best-effort docs input; tolerated like the Query error
	}

	// --- Auth endpoints ---
	authRows, err := ctx.DB.Query("SELECT path, table_name, username_column FROM of_auth_endpoints")
	if err == nil {
		defer authRows.Close()
		for authRows.Next() {
			var path, tableName, usernameCol string
			if authRows.Scan(&path, &tableName, &usernameCol) != nil {
				continue
			}

			loginSchema := map[string]any{
				"type": "object",
				"properties": map[string]any{
					usernameCol: map[string]any{"type": "string"},
					"password":  map[string]any{"type": "string", "format": "password"},
				},
				"required": []string{usernameCol, "password"},
			}
			schemas[componentName(path)+"Login"] = loginSchema

			// Build user response schema from table columns.
			userProps := map[string]any{"id": map[string]any{"type": "integer"}}
			if cols, ok := tableSchemas[tableName]; ok {
				for _, c := range cols {
					if strings.ToUpper(c.Type) == "PASSWORD" {
						continue
					}
					userProps[c.Name] = map[string]any{"type": sqlTypeToOpenAPI(c.Type)}
				}
			}
			schemas[componentName(path)+"User"] = map[string]any{"type": "object", "properties": userProps}

			tokenSchema := func() map[string]any {
				return map[string]any{"type": "object", "properties": map[string]any{"token": map[string]any{"type": "string"}}}
			}
			paths["/api/"+path+"/register"] = map[string]any{
				"post": map[string]any{
					"summary":     "Register new user",
					"operationId": "register_" + path,
					"requestBody": map[string]any{
						"required": true,
						"content":  jsonContent(schemaRef(componentName(path) + "Login")),
					},
					"responses": map[string]any{
						"201": jsonResp("Registered", tokenSchema()),
					},
				},
			}
			paths["/api/"+path+"/login"] = map[string]any{
				"post": map[string]any{
					"summary":     "Login",
					"operationId": "login_" + path,
					"requestBody": map[string]any{
						"required": true,
						"content":  jsonContent(schemaRef(componentName(path) + "Login")),
					},
					"responses": map[string]any{
						"200": jsonResp("Login successful", tokenSchema()),
						"401": map[string]any{"description": "Invalid credentials"},
					},
				},
			}
			paths["/api/"+path+"/me"] = map[string]any{
				"get": map[string]any{
					"summary":     "Get current user",
					"operationId": "me_" + path,
					"security":    bearerSecurity(),
					"responses": map[string]any{
						"200": jsonResp("Current user", schemaRef(componentName(path)+"User")),
						"401": map[string]any{"description": "Unauthorized"},
					},
				},
			}
		}
		_ = authRows.Err() // best-effort docs input; tolerated like the Query error
	}

	// --- Upload endpoints ---
	uploadRows, err := ctx.DB.Query("SELECT path, allowed_types, max_size_mb, requires_auth FROM of_upload_endpoints")
	if err == nil {
		defer uploadRows.Close()
		for uploadRows.Next() {
			var path string
			var allowedTypes sql.NullString
			var maxSizeMB sql.NullFloat64
			var requiresAuth bool
			if uploadRows.Scan(&path, &allowedTypes, &maxSizeMB, &requiresAuth) != nil {
				continue
			}
			desc := "Upload a file"
			if allowedTypes.Valid && allowedTypes.String != "" {
				desc += " (allowed: " + allowedTypes.String + ")"
			}
			uploadOp := map[string]any{
				"summary":     "Upload file to " + path,
				"operationId": "upload_" + path,
				"description": desc,
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{"multipart/form-data": map[string]any{
						"schema": map[string]any{
							"type":       "object",
							"properties": map[string]any{"file": map[string]any{"type": "string", "format": "binary"}},
							"required":   []string{"file"},
						},
					}},
				},
				"responses": map[string]any{
					"201": jsonResp("Upload successful", map[string]any{
						"type": "object",
						"properties": map[string]any{
							"url":      map[string]any{"type": "string"},
							"filename": map[string]any{"type": "string"},
							"size":     map[string]any{"type": "integer"},
							"type":     map[string]any{"type": "string"},
						},
					}),
				},
			}
			paths["/api/"+path+"/upload"] = map[string]any{"post": secure(uploadOp, requiresAuth)}
		}
		_ = uploadRows.Err() // best-effort docs input; tolerated like the Query error
	}

	// --- Stream endpoints ---
	streamRows, err := ctx.DB.Query("SELECT path, event_types, requires_auth FROM of_stream_endpoints")
	if err == nil {
		defer streamRows.Close()
		for streamRows.Next() {
			var path string
			var eventTypes sql.NullString
			var requiresAuth bool
			if streamRows.Scan(&path, &eventTypes, &requiresAuth) != nil {
				continue
			}
			streamOp := map[string]any{
				"summary":     "SSE stream for " + path,
				"operationId": "stream_" + path,
				"description": "Server-Sent Events stream for real-time updates",
				"responses": map[string]any{
					"200": map[string]any{
						"description": "SSE event stream",
						"content":     map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}},
					},
				},
			}
			paths["/api/"+path+"/stream"] = map[string]any{"get": secure(streamOp, requiresAuth)}
		}
		_ = streamRows.Err() // best-effort docs input; tolerated like the Query error
	}

	// --- WebSocket endpoints ---
	wsRows, err := ctx.DB.Query("SELECT path, requires_auth FROM of_ws_endpoints")
	if err == nil {
		defer wsRows.Close()
		for wsRows.Next() {
			var path string
			var requiresAuth bool
			if wsRows.Scan(&path, &requiresAuth) != nil {
				continue
			}
			wsOp := map[string]any{
				"summary":     "WebSocket connection for " + path,
				"operationId": "ws_" + path,
				"description": "Connect via ws(s)://host/api/" + path + "/ws?room=ROOM_NAME",
				"parameters":  []any{map[string]any{"name": "room", "in": "query", "schema": map[string]any{"type": "string"}, "description": "Room name for scoped messaging"}},
				"responses": map[string]any{
					"101": map[string]any{"description": "WebSocket upgrade"},
				},
			}
			paths["/api/"+path+"/ws"] = map[string]any{"get": secure(wsOp, requiresAuth)}
		}
		_ = wsRows.Err() // best-effort docs input; tolerated like the Query error
	}

	// Marshal the spec to JSON.
	specJSON, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return tools.Fail("%s", "failed to marshal spec: "+err.Error()), nil
	}

	// Save the spec as a file-based asset (api-docs.json).
	storagePath, err := store.WriteFileToDisk(ctx.DataDirOrDefault(), ctx.ProjectID, "assets", "api-docs.json", specJSON)
	if err != nil {
		return tools.Fail("%s", "failed to write spec file: "+err.Error()), nil
	}
	_, err = ctx.DB.Exec(
		`INSERT INTO of_files (kind, path, storage, content_type, size, disk_path, scope, metadata, is_deleted, updated_at)
		 VALUES ('file', ?, 'disk', ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP)
		 ON CONFLICT(kind, path) DO UPDATE SET
		   storage = 'disk',
		   body = '',
		   content_type = excluded.content_type,
		   size = excluded.size,
		   disk_path = excluded.disk_path,
		   metadata = excluded.metadata,
		   updated_at = CURRENT_TIMESTAMP`,
		"api-docs.json", "application/json", len(specJSON), storagePath, "global",
		`{"alt_text":"OpenAPI 3.0 specification"}`,
	)
	if err != nil {
		return tools.Fail("%s", "failed to save spec asset: "+err.Error()), nil
	}

	// The machine-readable spec at /api/docs.json is the only docs artifact: a
	// rendered viewer page would load its bundle from a third-party CDN at runtime,
	// which breaks the offline/self-contained guarantee and injects a third-party
	// script into every generated app. Import /api/docs.json into any OpenAPI viewer
	// instead. Best-effort: drop any /api/docs page so it cannot serve one.
	_, _ = store.ArtifactDelete(ctx.DB, "page", "/api/docs", ctx.ChangedBy())

	endpointCount := len(paths)
	schemaCount := len(schemas)

	return tools.Ok(map[string]any{
		"message":    fmt.Sprintf("Generated OpenAPI spec with %d paths and %d schemas, served at /api/docs.json (open it in any OpenAPI viewer)", endpointCount, schemaCount),
		"spec_asset": "api-docs.json",
		"spec_url":   "/api/docs.json",
	}), nil
}
