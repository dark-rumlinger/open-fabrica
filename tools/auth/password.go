package auth

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"openfabrica/internal/endpoints"
	"openfabrica/store"
	"openfabrica/tools"
	"openfabrica/tools/api"

	"openfabrica/security"
)

// Password (JWT) auth handlers for manage_auth. Schema lives in manage_auth.go.

// Auth endpoint actions

// parsePublicColumns normalizes the public_columns argument into a list of column
// names surfaced in /me. It accepts both the documented string form ("role") and
// the object form ({"name":"role"}) the model sometimes emits - echoing
// manage_schema's column_defs shape. A non-string element is rejected rather than
// silently dropped: a silent drop could empty public_columns entirely, so /me
// would omit the role column and requireRole would bounce every logged-in user
// home. Returns a non-empty error string when an element is malformed or names an
// invalid column so the caller fails loudly instead of degrading silently.
func parsePublicColumns(raw []any) ([]string, string) {
	var cols []string
	for i, c := range raw {
		var name string
		switch v := c.(type) {
		case string:
			name = v
		case map[string]any:
			name, _ = v["name"].(string)
		}
		if name == "" {
			return nil, fmt.Sprintf("public_columns[%d] must be a column name string (e.g. \"role\") or {\"name\":\"role\"}; got %v", i, c)
		}
		if !store.ValidColumnName.MatchString(name) {
			return nil, fmt.Sprintf("invalid public_column name: %s", name)
		}
		cols = append(cols, name)
	}
	return cols, ""
}

func epCreateAuth(ctx *tools.ToolContext, a *AuthCreate) (*tools.Result, error) {
	tableName := a.TableName
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	usernameCol := a.UsernameColumn
	passwordCol := a.PasswordColumn
	if passwordCol == "" {
		passwordCol = "password"
	}
	rateLimit := a.RateLimit
	// Honoured by the runtime token issuer (server/public/auth.go); it was read there
	// but settable nowhere, pinning every project to the schema default of 24h.
	jwtExpiryHours := a.JWTExpiryHours
	if jwtExpiryHours <= 0 {
		jwtExpiryHours = 24
	}
	if rateLimit <= 0 {
		rateLimit = 60
	}

	if tableName == "" || path == "" || usernameCol == "" {
		return tools.Fail("table_name, endpoint_path, and username_column are required"), nil
	}
	// The auth path is spliced verbatim into the platform-managed js/auth.js
	// bootstrap (a global script on every page) and into magic-link email hrefs,
	// so restrict it to the safe endpoint-path charset - a path carrying quotes
	// or other metacharacters would otherwise break out into executable JS.
	if msg := tools.ValidateEndpointPathCharset(path); msg != "" {
		return tools.Fail("%s", msg), nil
	}

	// Skip if auth endpoint already exists (prevents duplicate creation during BUILD).
	var existingCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_auth_endpoints WHERE path = ?", path).Scan(&existingCount); err == nil && existingCount > 0 {
		return tools.Ok(map[string]any{
			"endpoint_path":   path,
			"already_existed": true,
			"message":         "Auth endpoint /api/" + path + " already exists, so nothing was created and NO option in this call was applied. Change it with manage_auth action=update.",
		}), nil
	}

	if !store.ValidColumnName.MatchString(usernameCol) {
		return tools.Fail("invalid username_column name: %s", usernameCol), nil
	}
	if !store.ValidColumnName.MatchString(passwordCol) {
		return tools.Fail("invalid password_column name: %s", passwordCol), nil
	}

	// Verify the table exists and has a PASSWORD column.
	secureCols, err := store.LoadSecureColumns(ctx.DB, tableName)
	if err != nil || secureCols == nil {
		return tools.Fail("table %q not found in dynamic tables registry", tableName), nil
	}
	if kind, ok := secureCols[passwordCol]; !ok || kind != "hash" {
		return tools.Fail("column %q in table %q is not a PASSWORD column", passwordCol, tableName), nil
	}

	// Verify username_column exists in the actual table schema.
	var schemaDef string
	if err := ctx.DB.QueryRow(
		"SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?", tableName,
	).Scan(&schemaDef); err != nil {
		return tools.Fail("could not load schema for table %q", tableName), nil
	}
	var schemaMap map[string]any
	if err := json.Unmarshal([]byte(schemaDef), &schemaMap); err != nil {
		return tools.Fail("invalid schema_def for table %q", tableName), nil
	}
	if _, ok := schemaMap[usernameCol]; !ok {
		var available []string
		for col := range schemaMap {
			if col != passwordCol {
				available = append(available, col)
			}
		}
		return tools.Fail(
			"username_column %q does not exist in table %q, available columns: %s",
			usernameCol, tableName, strings.Join(available, ", ")), nil
	}

	// Verify the physical SQLite table actually exists (not just the registry entry).
	var colCount int
	if err := ctx.DB.QueryRow(
		fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s')", tableName),
	).Scan(&colCount); err != nil || colCount == 0 {
		return tools.Fail(
			"table %q is registered but does not exist as a physical table, recreate it with manage_schema create_table",
			tableName), nil
	}

	// Build public columns JSON.
	publicColumnsJSON := "[]"
	if len(a.PublicColumns) > 0 {
		pubCols, errMsg := parsePublicColumns(a.PublicColumns)
		if errMsg != "" {
			return tools.Fail("%s", errMsg), nil
		}
		data, _ := json.Marshal(pubCols)
		publicColumnsJSON = string(data)
	}

	signupColumnsJSON := "[]"
	if len(a.SignupColumns) > 0 {
		cols, errMsg := signupColumnList(a.SignupColumns, schemaMap)
		if errMsg != "" {
			return tools.Fail("%s", errMsg), nil
		}
		data, _ := json.Marshal(cols)
		signupColumnsJSON = string(data)
	}

	// RBAC: default role and role column.
	defaultRole := "user"
	if a.DefaultRole != "" {
		defaultRole = a.DefaultRole
	}
	roleColumn := "role"
	if a.RoleColumn != "" {
		if !store.ValidColumnName.MatchString(a.RoleColumn) {
			return tools.Fail("invalid role_column name: %s", a.RoleColumn), nil
		}
		roleColumn = a.RoleColumn
	}

	// Ensure role_column exists in the table, auto-add if missing. Presence is
	// checked against the physical table, not just the registry: a column the
	// registry lost track of needs a registry repair, not a doomed ALTER.
	if _, ok := schemaMap[roleColumn]; !ok {
		var roleColCount int
		if err := ctx.DB.QueryRow(
			fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?", tableName),
			roleColumn,
		).Scan(&roleColCount); err != nil {
			return tools.Fail("could not inspect table %q: %v", tableName, err), nil
		}
		if roleColCount == 0 {
			_, alterErr := ctx.DB.Exec(
				fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s TEXT DEFAULT ''", tableName, roleColumn))
			// A duplicate-column error means the column exists after all
			// (concurrent add); treat that as success and repair the registry.
			if alterErr != nil && !strings.Contains(alterErr.Error(), "duplicate column") {
				return tools.Fail(
					"could not add role_column %q to table %q: %v",
					roleColumn, tableName, alterErr), nil
			}
		}
		schemaMap[roleColumn] = "TEXT"
		updatedSchema, _ := json.Marshal(schemaMap)
		if _, err := ctx.DB.Exec("UPDATE of_dynamic_tables SET schema_def = ? WHERE table_name = ?",
			string(updatedSchema), tableName); err != nil {
			slog.Warn("failed to update schema_def after adding role column", "table", tableName, "error", err)
		}
	}

	// Claim the auth action sub-routes in the endpoint spine (register/login/…).
	if err := api.ClaimEndpoint(ctx, endpoints.KindAuth, path, nil); err != nil {
		return tools.FailErr(err), nil
	}

	// Insert into of_auth_endpoints table.
	result, err := ctx.DB.Exec(
		`INSERT INTO of_auth_endpoints (table_name, path, username_column, password_column, public_columns, signup_columns, default_role, role_column, rate_limit, jwt_expiry_hours)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   table_name = excluded.table_name,
		   username_column = excluded.username_column,
		   password_column = excluded.password_column,
		   public_columns = excluded.public_columns,
		   signup_columns = excluded.signup_columns,
		   default_role = excluded.default_role,
		   role_column = excluded.role_column,
		   rate_limit = excluded.rate_limit,
		   jwt_expiry_hours = excluded.jwt_expiry_hours`,
		tableName, path, usernameCol, passwordCol, publicColumnsJSON, signupColumnsJSON, defaultRole, roleColumn, rateLimit, jwtExpiryHours,
	)
	if err != nil {
		return nil, fmt.Errorf("creating auth endpoint: %w", err)
	}

	id, _ := result.LastInsertId()

	// Emit the platform-managed auth bootstrap (js/auth.js, an ES module exporting
	// requireAuth/requireRole/redirectIfLoggedIn/apiFetch/logout) with this endpoint's path
	// substituted in. Global scope -> the renderer auto-injects it on every page;
	// platform_managed -> the model can't edit or delete it, so the load-order
	// guarantees that prevent the /admin <-> /login redirect loop are frozen.
	helperEmitted := emitAuthClientHelper(ctx.DB, path)

	created := map[string]any{
		"id":            id,
		"endpoint_path": path,
		"routes": []string{
			fmt.Sprintf("POST /api/%s/register", path),
			fmt.Sprintf("POST /api/%s/login", path),
			fmt.Sprintf("GET /api/%s/me", path),
			fmt.Sprintf("POST /api/%s/refresh", path),
		},
		"table":           tableName,
		"username_column": usernameCol,
		"password_column": passwordCol,
		"default_role":    defaultRole,
		"role_column":     roleColumn,
		"rate_limit":      rateLimit,
		"client_helper":   "js/auth.js - platform-managed ES module, imported by the specifier 'auth'",
		// The helper NAMES are the auth law's to list (contracts.IDAuthWindowAPI), and it is
		// already transcluded into every prompt - a second list here drifts from it, which is
		// how this result came to advertise five helpers where the law names seven.
		"next_step": "The platform has emitted js/auth.js as an ES module - import its helpers by specifier ('auth') per the auth law on your prompt. Do not author your own auth bootstrap. See manage_guides action=recall slug=auth_contract for the step-up flows.",
	}
	if !helperEmitted {
		// The endpoint exists, but pages importing 'auth' would get a 404 module.
		created["warning"] = "The endpoint was created but js/auth.js could NOT be written, so `import ... from 'auth'` fails on every page. Check manage_files for a conflicting js/auth.js, then run manage_auth create again (it is safe to repeat)."
		delete(created, "next_step")
	}
	return tools.Ok(created), nil
}

func epUpdateAuth(ctx *tools.ToolContext, a *AuthUpdate) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(a.EndpointPath)
	if path == "" {
		return tools.Fail("endpoint_path is required"), nil
	}

	setClauses := []string{}
	var vals []any

	if a.DefaultRole != "" {
		setClauses = append(setClauses, "default_role = ?")
		vals = append(vals, a.DefaultRole)
	}
	if len(a.PublicColumns) > 0 {
		cs, errMsg := parsePublicColumns(a.PublicColumns)
		if errMsg != "" {
			return tools.Fail("%s", errMsg), nil
		}
		b, _ := json.Marshal(cs)
		setClauses = append(setClauses, "public_columns = ?")
		vals = append(vals, string(b))
	}
	if len(a.SignupColumns) > 0 {
		var table string
		_ = ctx.DB.QueryRow("SELECT table_name FROM of_auth_endpoints WHERE path = ?", path).Scan(&table)
		cols, errMsg := signupColumnList(a.SignupColumns, tableSchemaMap(ctx, table))
		if errMsg != "" {
			return tools.Fail("%s", errMsg), nil
		}
		b, _ := json.Marshal(cols)
		setClauses = append(setClauses, "signup_columns = ?")
		vals = append(vals, string(b))
	}
	if a.RoleColumn != "" {
		if !store.ValidColumnName.MatchString(a.RoleColumn) {
			return tools.Fail("invalid role_column name: %s", a.RoleColumn), nil
		}
		setClauses = append(setClauses, "role_column = ?")
		vals = append(vals, a.RoleColumn)
	}
	if a.UsernameColumn != "" {
		if !store.ValidColumnName.MatchString(a.UsernameColumn) {
			return tools.Fail("invalid username_column name: %s", a.UsernameColumn), nil
		}
		setClauses = append(setClauses, "username_column = ?")
		vals = append(vals, a.UsernameColumn)
	}
	if a.PasswordColumn != "" {
		if !store.ValidColumnName.MatchString(a.PasswordColumn) {
			return tools.Fail("invalid password_column name: %s", a.PasswordColumn), nil
		}
		setClauses = append(setClauses, "password_column = ?")
		vals = append(vals, a.PasswordColumn)
	}
	if a.RateLimit != 0 {
		setClauses = append(setClauses, "rate_limit = ?")
		vals = append(vals, a.RateLimit)
	}
	if a.JWTExpiryHours > 0 {
		setClauses = append(setClauses, "jwt_expiry_hours = ?")
		vals = append(vals, a.JWTExpiryHours)
	}

	if len(setClauses) == 0 {
		return tools.Fail("no fields to update"), nil
	}

	vals = append(vals, path)
	query := fmt.Sprintf("UPDATE of_auth_endpoints SET %s WHERE path = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("updating auth endpoint: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.Fail("auth endpoint %q not found", path), nil
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "updated": true}), nil
}

func epListAuth(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(
		"SELECT id, path, table_name, username_column, password_column, public_columns, rate_limit, created_at FROM of_auth_endpoints ORDER BY path",
	)
	if err != nil {
		return nil, fmt.Errorf("listing auth endpoints: %w", err)
	}
	defer rows.Close()

	var endpoints []map[string]any
	for rows.Next() {
		var id, rateLimit int
		var path, tableName, usernameCol, passwordCol, publicCols string
		var createdAt any
		if err := rows.Scan(&id, &path, &tableName, &usernameCol, &passwordCol, &publicCols, &rateLimit, &createdAt); err != nil {
			continue
		}
		endpoints = append(endpoints, map[string]any{
			"id":              id,
			"endpoint_path":   path,
			"table_name":      tableName,
			"username_column": usernameCol,
			"password_column": passwordCol,
			"public_columns":  publicCols,
			"rate_limit":      rateLimit,
			"created_at":      createdAt,
			"routes": []string{
				fmt.Sprintf("POST /api/%s/register", path),
				fmt.Sprintf("POST /api/%s/login", path),
				fmt.Sprintf("GET /api/%s/me", path),
				fmt.Sprintf("POST /api/%s/refresh", path),
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing auth endpoints: %w", err)
	}

	return tools.Ok(endpoints), nil
}

func epDeleteAuth(ctx *tools.ToolContext, a *AuthDelete) (*tools.Result, error) {
	return api.DeleteEndpointByPath(ctx, "of_auth_endpoints", "auth endpoint", "manage_auth", "delete", a.EndpointPath)
}

func epVerifyPassword(ctx *tools.ToolContext, a *AuthVerifyPassword) (*tools.Result, error) {
	tableName := a.TableName
	password := a.Password
	// The column the auth endpoint over this table hashes into; "password" when no
	// endpoint names one. A hardcoded "password" missed a renamed column and then
	// reported the scan error as a missing row.
	passwordCol := "password"
	var configured string
	if ctx.DB.QueryRow("SELECT COALESCE(password_column,'') FROM of_auth_endpoints WHERE table_name = ? LIMIT 1", tableName).Scan(&configured) == nil && configured != "" {
		passwordCol = configured
	}

	if tableName == "" || password == "" || a.UserID <= 0 {
		return tools.Fail("table_name, user_id, and password are required"), nil
	}

	physicalName, err := store.SanitizedTableName(tableName)
	if err != nil {
		return tools.FailErr(err), nil
	}

	if !store.ValidColumnName.MatchString(passwordCol) {
		return tools.Fail("invalid column name: %s", passwordCol), nil
	}

	// Fetch the hashed password from the row.
	var hashedPassword string
	err = ctx.DB.QueryRow(
		fmt.Sprintf("SELECT %s FROM %s WHERE id = ?", passwordCol, physicalName),
		int64(a.UserID),
	).Scan(&hashedPassword)
	if err != nil {
		return tools.NotFound(fmt.Sprintf("%s row with id", tableName), fmt.Sprint(a.UserID)), nil
	}

	match := checkPasswordHash(password, hashedPassword)

	return tools.Ok(map[string]any{
		"match": match,
	}), nil
}

// checkPasswordHash compares a plaintext password against a bcrypt hash.
func checkPasswordHash(password, hash string) bool {
	return security.CheckPassword(password, hash)
}

// signupColumnList validates the requested signup columns against the table
// schema. The single entry "*" clears the list (back to the default filter).
func signupColumnList(cols []string, schema map[string]any) ([]string, string) {
	if len(cols) == 1 && cols[0] == "*" {
		return []string{}, ""
	}
	for _, c := range cols {
		if !store.ValidColumnName.MatchString(c) {
			return nil, fmt.Sprintf("invalid signup_columns name: %s", c)
		}
		if schema != nil {
			if _, ok := schema[c]; !ok {
				return nil, fmt.Sprintf("signup_columns: %q is not a column of the user table", c)
			}
		}
	}
	return cols, ""
}

// tableSchemaMap reads a dynamic table's registered column map, or nil when it
// cannot be read.
func tableSchemaMap(ctx *tools.ToolContext, table string) map[string]any {
	var schemaDef string
	if err := ctx.DB.QueryRow("SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?", table).Scan(&schemaDef); err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(schemaDef), &m) != nil {
		return nil
	}
	return m
}
