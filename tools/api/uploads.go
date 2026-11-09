package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"openfabrica/internal/endpoints"
	"openfabrica/store"
	"openfabrica/tools"
)

// File-upload handlers, MIME constants, and the allow-list validator for
// manage_uploads. Schema lives in manage_uploads.go.

// defaultUploadAllowedTypes is applied when create_upload is called without
// allowed_types. Covers the common "user uploads an avatar or document" case.
var defaultUploadAllowedTypes = []string{
	"image/*",
	"application/pdf",
	"text/*",
	"application/json",
	"application/zip",
}

// dangerousUploadMIMEs is rejected at endpoint creation. The runtime never
// dispatches binaries, but allowing executable MIME types invites browsers and
// downstream tools to treat the asset as runnable.
var dangerousUploadMIMEs = map[string]bool{
	"application/x-msdownload":                      true,
	"application/x-msdos-program":                   true,
	"application/x-executable":                      true,
	"application/x-mach-binary":                     true,
	"application/x-dosexec":                         true,
	"application/x-sh":                              true,
	"application/x-bat":                             true,
	"application/x-bsh":                             true,
	"application/x-csh":                             true,
	"application/java-archive":                      true,
	"application/vnd.microsoft.portable-executable": true,
}

// validateUploadAllowedTypes checks each entry against the deny-list. Globs
// containing "executable" or "x-msi" are rejected too, even if not in the map.
func validateUploadAllowedTypes(types []string) error {
	for _, t := range types {
		lower := strings.ToLower(strings.TrimSpace(t))
		if lower == "" {
			return fmt.Errorf("allowed_types contains an empty entry")
		}
		if dangerousUploadMIMEs[lower] {
			return fmt.Errorf("MIME type %q is not allowed (executable content)", t)
		}
		if strings.Contains(lower, "executable") || strings.Contains(lower, "x-msi") {
			return fmt.Errorf("MIME type %q is not allowed (executable content)", t)
		}
	}
	return nil
}

// autoFilledUploadCols are the file-metadata columns the upload handler populates
// on every upload (server/public/upload.go). A NOT NULL column outside this set
// with no default makes every upload fail the constraint, so create and update
// both reject the binding up front. The endpoint's configured owner/tenant
// columns are filled too, but only when configured, so they join the set per
// endpoint rather than living here (see uploadFilledCols).
var autoFilledUploadCols = map[string]bool{
	"filename": true, "url": true, "content_type": true, "size": true,
}

// uploadFilledCols returns every column the handler fills for THIS endpoint: the
// file metadata, plus the owner/tenant columns it stamps from the caller's claims
// when the endpoint is configured with them. Reading them back keeps the gate in
// step with the runtime, so a binding the handler would satisfy is not rejected
// as unfillable.
func uploadFilledCols(ctx *tools.ToolContext, path string) map[string]bool {
	filled := make(map[string]bool, len(autoFilledUploadCols)+2)
	for c := range autoFilledUploadCols {
		filled[c] = true
	}
	var owner, tenant string
	if err := ctx.DB.QueryRow(
		"SELECT COALESCE(owner_column,''), COALESCE(tenant_column,'') FROM of_upload_endpoints WHERE path = ?", path,
	).Scan(&owner, &tenant); err != nil {
		return filled // no row yet (create), or no such endpoint: file metadata only
	}
	if owner != "" {
		filled[owner] = true
	}
	if tenant != "" {
		filled[tenant] = true
	}
	return filled
}

// unfillableUploadColumns returns the table's NOT NULL, no-default columns the
// upload cannot supply. store.RequiredColumns already excludes anything with a
// default - including a `generate` expression - so a non-empty result means
// every upload into this table fails the constraint.
func unfillableUploadColumns(ctx *tools.ToolContext, path, tableName string) []string {
	required, err := store.RequiredColumns(ctx.DB, tableName)
	if err != nil {
		return nil
	}
	filled := uploadFilledCols(ctx, path)
	var cols []string
	for col := range required {
		if !filled[col] {
			cols = append(cols, col)
		}
	}
	sort.Strings(cols)
	return cols
}

// unfillableUploadError explains the rejection in terms of moves that exist.
// Relaxing NOT NULL is never among them: manage_schema alter_table offers only
// add_columns/drop_columns, so a remedy phrased as "make the column nullable" is
// one no caller can carry out.
func unfillableUploadError(tableName string, cols []string) string {
	return fmt.Sprintf(
		"table %q has NOT NULL column(s) %v the upload cannot fill - it fills filename/url/content_type/size (plus this endpoint's owner/tenant column when one is configured), so every upload would fail the constraint. The binding was rejected and nothing changed. Pick one:\n"+
			"  (a) the values come from the user (a title, a caption): omit table_name (or pass \"none\") to store the file on disk, then create the row from the returned url with this table's own create_api POST, sending those fields in the same insert - no server function needed;\n"+
			"  (b) the values are server-generated (a share code, an expiry): declare the column with `generate` in manage_schema create_table;\n"+
			"  (c) the values are optional: create the table without NOT NULL on them.\n"+
			"Note: alter_table cannot remove NOT NULL from an existing column - (b) and (c) mean creating the table in its final shape.",
		tableName, cols,
	)
}

// isNoTableSentinel reports whether a table_name argument is one of the reserved
// "store the file on disk, don't insert a linked row" words. The runtime already
// treats an empty table_name as disk-only (server/public/upload.go), but the
// update tool cannot express "clear the binding" via the empty string (empty
// means "leave unchanged"), so these sentinels give a way to detach - and they
// match what a caller naturally reaches for when it wants no linked table.
func isNoTableSentinel(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "null", "-":
		return true
	}
	return false
}

// validateUploadOwnerColumn checks an owner_column against the endpoint it will
// belong to, and returns a rejection Result (or nil) rather than an error so both
// create and update refuse on the same terms. It is shared precisely because the two
// actions must agree: an endpoint that can only be SCOPED at create is an endpoint
// whose scoping the model cannot add later, which is how this setting first shipped.
//
// owner_column is STAMPED by the runtime from the authenticated user
// (server/public/upload.go), so all three preconditions are load-bearing: a table to
// stamp onto, a real column on it, and an authenticated request to read the user
// from. Without auth the stamp has no user and silently writes nothing - a
// my-uploads-only endpoint that quietly shows everyone everything.
func validateUploadOwnerColumn(ctx *tools.ToolContext, ownerColumn, tableName string, requiresAuth bool) *tools.Result {
	if ownerColumn == "" {
		return nil
	}
	if tableName == "" {
		return tools.FailCode(tools.CodeBadArg, "owner_column needs table_name: with no linked row there is nothing to stamp the uploader onto")
	}
	if !requiresAuth {
		return tools.FailCode(tools.CodeBadArg, "owner_column needs requires_auth=true: the uploader is read from the request's auth token, so on an unauthenticated endpoint the column is written empty and every user sees every file")
	}
	if !store.ValidColumnName.MatchString(ownerColumn) {
		return tools.FailCode(tools.CodeBadArg, "invalid owner_column name: %s", ownerColumn)
	}
	if cols, err := store.GetTableColumns(ctx.DB, tableName); err == nil && len(cols) > 0 && !cols[ownerColumn] {
		return tools.FailCode(tools.CodeBadArg, "owner_column %q does not exist on table %q", ownerColumn, tableName)
	}
	return nil
}

// Upload endpoint actions

// epCreateUpload takes typed args straight from UploadCreate (no map
// round-trip). allowedTypesRaw is the declared []interface{} arg; entries that
// are not strings are skipped, matching the prior map behaviour.
func epCreateUpload(ctx *tools.ToolContext, rawPath, tableName string, requiresAuth bool, rateLimit int, allowedTypesRaw []any, maxSizeMB int, ownerColumn string) (*tools.Result, error) {
	path, errRes := tools.ValidateAndNormalizeEndpointPath(rawPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" {
		return tools.FailCode(tools.CodeMissingArg, "endpoint_path is required"), nil
	}

	// Skip if upload endpoint already exists (prevents duplicate creation during BUILD).
	var existingCount int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_upload_endpoints WHERE path = ?", path).Scan(&existingCount); err == nil && existingCount > 0 {
		return tools.Ok(map[string]any{
			"endpoint_path":   path,
			"already_existed": true,
			"message":         "Upload endpoint /api/" + path + " already exists, so nothing was created and NO option in this call was applied. Change its config with manage_uploads action=update.",
		}), nil
	}

	// Parse allowed_types (default: common file types).
	allowedTypes := append([]string(nil), defaultUploadAllowedTypes...)
	if len(allowedTypesRaw) > 0 {
		allowedTypes = nil
		for _, t := range allowedTypesRaw {
			if ts, ok := t.(string); ok {
				allowedTypes = append(allowedTypes, ts)
			}
		}
	}
	if len(allowedTypes) == 0 {
		return tools.FailCode(tools.CodeBadArg, "allowed_types must not be empty"), nil
	}
	if err := validateUploadAllowedTypes(allowedTypes); err != nil {
		return tools.FailErr(err), nil
	}
	allowedTypesJSON, _ := json.Marshal(allowedTypes)

	if maxSizeMB <= 0 {
		maxSizeMB = 5
	}
	if maxSizeMB > endpoints.MaxUploadSizeMB {
		maxSizeMB = endpoints.MaxUploadSizeMB
	}
	if rateLimit <= 0 {
		rateLimit = 60
	}

	// "none"/"null"/"-" mean disk-only: store the file, insert no linked row.
	// Normalize to the empty binding the runtime already treats as disk-only.
	if isNoTableSentinel(tableName) {
		tableName = ""
	}

	if tableName != "" && !store.ValidTableName.MatchString(tableName) {
		return tools.FailCode(tools.CodeBadArg, "invalid table_name %q (must be alphanumeric/underscore, starting with a letter)", tableName), nil
	}

	// Validate the linked table and inspect its columns. A non-existent table
	// (PRAGMA returns zero columns, no error) is rejected up front: binding to
	// it would 500 every upload with "no such table" at request time.
	//   - missingCols: the four auto-insert columns the table lacks. Those are
	//     silently dropped at request time, which can push the engine to
	//     drop+recreate the table; surface it so it fixes the schema instead.
	//   - unfillable columns: NOT NULL, no-default columns the upload can't fill.
	//     Each makes every upload fail, so the binding is rejected before
	//     anything commits rather than shipping an endpoint that cannot work.
	var missingCols []string
	if tableName != "" {
		cols, err := store.GetTableColumns(ctx.DB, tableName)
		if err == nil && len(cols) == 0 {
			return tools.FailCode(tools.CodeNotFound,
				"table_name %q does not exist - create it first with manage_schema, or omit table_name (or pass \"none\") to store the file on disk without a linked row", tableName), nil
		}
		if err == nil {
			for _, required := range []string{"filename", "url", "content_type", "size"} {
				if !cols[required] {
					missingCols = append(missingCols, required)
				}
			}
			if unfillable := unfillableUploadColumns(ctx, path, tableName); len(unfillable) > 0 {
				return &tools.Result{
					Success: false,
					Error:   unfillableUploadError(tableName, unfillable),
					Data:    map[string]any{"unfillable_required_columns": unfillable},
				}, nil
			}
		}
	}

	if rej := validateUploadOwnerColumn(ctx, ownerColumn, tableName, requiresAuth); rej != nil {
		return rej, nil
	}

	// Claim the spine row and write the config row in one transaction, so a
	// failure leaves neither. A spine row without a config row (or the reverse)
	// makes the route stop resolving and the client sees a connection reset.
	tx, err := ctx.DB.Begin()
	if err != nil {
		return nil, fmt.Errorf("creating upload endpoint: %w", err)
	}
	if err := ClaimEndpointTx(tx, endpoints.KindUpload, path, nil); err != nil {
		_ = tx.Rollback()
		return tools.FailErr(err), nil
	}
	if _, err := tx.Exec(
		`INSERT INTO of_upload_endpoints (path, allowed_types, max_size_mb, requires_auth, rate_limit, table_name, owner_column)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		   allowed_types = excluded.allowed_types,
		   max_size_mb = excluded.max_size_mb,
		   requires_auth = excluded.requires_auth,
		   rate_limit = excluded.rate_limit,
		   table_name = excluded.table_name,
		   owner_column = excluded.owner_column`,
		path, string(allowedTypesJSON), maxSizeMB, requiresAuth, rateLimit, tableName, tools.NullIfEmpty(ownerColumn),
	); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("creating upload endpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("creating upload endpoint: %w", err)
	}

	data := map[string]any{
		"endpoint_path": path,
		"route":         fmt.Sprintf("POST /api/%s/upload", path),
		"allowed_types": allowedTypes,
		"max_size_mb":   maxSizeMB,
		"requires_auth": requiresAuth,
		"rate_limit":    rateLimit,
		"table_name":    tableName,
	}
	if len(missingCols) > 0 {
		data["warning"] = fmt.Sprintf(
			"Table %q is missing column(s) %v - every upload will fail with a 500 until the table has all of filename, url, content_type and size. Fix with manage_schema alter_table add_columns (do NOT drop_table - that's destructive and unnecessary here).",
			tableName, missingCols,
		)
		data["missing_required_columns"] = missingCols
	}
	return tools.Ok(data), nil
}

// epUpdateUpload takes typed args straight from UploadUpdate. Each optional
// field is patched only when set: empty allowedTypesRaw / zero maxSizeMB /
// empty tableName mean "leave unchanged", matching the prior merge-map shape
// where those keys were simply absent. requires_auth is a tri-state pointer
// so `requires_auth: false` can reopen an over-gated endpoint - a plain bool
// could never express false.
func epUpdateUpload(ctx *tools.ToolContext, rawPath, tableName string, requiresAuth *bool, rateLimit int, allowedTypesRaw []any, maxSizeMB int, ownerColumn string) (*tools.Result, error) {
	path := tools.NormalizeEndpointPath(rawPath)
	if path == "" {
		return tools.FailCode(tools.CodeMissingArg, "endpoint_path is required"), nil
	}

	// owner_column is validated against the endpoint as it will be AFTER this call,
	// not as it is now: table_name and requires_auth may be changing in the same
	// update, and checking the stale values would accept a combination that never
	// works (or refuse one that does).
	var curTable string
	var curAuth bool
	if err := ctx.DB.QueryRow(
		"SELECT COALESCE(table_name, ''), requires_auth FROM of_upload_endpoints WHERE path = ?", path,
	).Scan(&curTable, &curAuth); err != nil {
		return tools.NotFound("upload endpoint", path), nil
	}
	nextTable, nextAuth := curTable, curAuth
	if isNoTableSentinel(tableName) {
		nextTable = ""
	} else if tableName != "" {
		nextTable = tableName
	}
	if requiresAuth != nil {
		nextAuth = *requiresAuth
	}

	setClauses := []string{}
	var vals []any

	if len(allowedTypesRaw) > 0 {
		var ts []string
		for _, t := range allowedTypesRaw {
			if s, ok := t.(string); ok {
				ts = append(ts, s)
			}
		}
		if len(ts) == 0 {
			return tools.FailCode(tools.CodeBadArg, "allowed_types must not be empty"), nil
		}
		if err := validateUploadAllowedTypes(ts); err != nil {
			return tools.FailErr(err), nil
		}
		b, _ := json.Marshal(ts)
		setClauses = append(setClauses, "allowed_types = ?")
		vals = append(vals, string(b))
	}
	if maxSizeMB != 0 {
		size := maxSizeMB
		if size > endpoints.MaxUploadSizeMB {
			size = endpoints.MaxUploadSizeMB
		}
		if size < 1 {
			size = 1
		}
		setClauses = append(setClauses, "max_size_mb = ?")
		vals = append(vals, size)
	}
	if requiresAuth != nil {
		setClauses = append(setClauses, "requires_auth = ?")
		vals = append(vals, *requiresAuth)
	}
	if rateLimit > 0 {
		setClauses = append(setClauses, "rate_limit = ?")
		vals = append(vals, rateLimit)
	}
	if isNoTableSentinel(tableName) {
		// Detach: clear the binding so the endpoint stores files on disk only.
		// This is the sole way to unbind an endpoint after create - an empty
		// table_name means "leave unchanged", so a sentinel is required.
		setClauses = append(setClauses, "table_name = ?")
		vals = append(vals, "")
	} else if tableName != "" {
		if !store.ValidTableName.MatchString(tableName) {
			return tools.FailCode(tools.CodeBadArg, "invalid table_name %q (must be alphanumeric/underscore, starting with a letter)", tableName), nil
		}
		if cols, err := store.GetTableColumns(ctx.DB, tableName); err == nil && len(cols) == 0 {
			return tools.FailCode(tools.CodeNotFound,
				"table_name %q does not exist - create it first with manage_schema, or pass \"none\" to store files on disk without a linked row", tableName), nil
		}
		if unfillable := unfillableUploadColumns(ctx, path, tableName); len(unfillable) > 0 {
			return &tools.Result{
				Success: false,
				Code:    tools.CodeBadArg,
				Error:   unfillableUploadError(tableName, unfillable),
				Data:    map[string]any{"unfillable_required_columns": unfillable},
			}, nil
		}
		setClauses = append(setClauses, "table_name = ?")
		vals = append(vals, tableName)
	}

	// A sentinel clears the scoping, mirroring how table_name detaches: an empty
	// owner_column means "leave unchanged", so there has to be a way to say "none".
	if isNoTableSentinel(ownerColumn) {
		setClauses = append(setClauses, "owner_column = ?")
		vals = append(vals, "")
	} else if ownerColumn != "" {
		if rej := validateUploadOwnerColumn(ctx, ownerColumn, nextTable, nextAuth); rej != nil {
			return rej, nil
		}
		setClauses = append(setClauses, "owner_column = ?")
		vals = append(vals, ownerColumn)
	}

	if len(setClauses) == 0 {
		return tools.FailCode(tools.CodeBadArg, "no fields to update"), nil
	}

	vals = append(vals, path)
	query := fmt.Sprintf("UPDATE of_upload_endpoints SET %s WHERE path = ?", strings.Join(setClauses, ", "))
	res, err := ctx.DB.Exec(query, vals...)
	if err != nil {
		return nil, fmt.Errorf("updating upload endpoint: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.NotFound("upload endpoint", path), nil
	}
	return tools.Ok(map[string]any{"endpoint_path": path, "updated": true}), nil
}

func epListUpload(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query("SELECT id, path, table_name, allowed_types, max_size_mb, requires_auth, rate_limit, owner_column, created_at FROM of_upload_endpoints ORDER BY path")
	if err != nil {
		return nil, fmt.Errorf("listing upload endpoints: %w", err)
	}
	defer rows.Close()
	var endpoints []map[string]any
	for rows.Next() {
		var id, maxSizeMB, rateLimit int
		var path string
		var tableName, allowedTypes, ownerColumn sql.NullString
		var requiresAuth bool
		var createdAt time.Time
		if rows.Scan(&id, &path, &tableName, &allowedTypes, &maxSizeMB, &requiresAuth, &rateLimit, &ownerColumn, &createdAt) != nil {
			continue
		}
		var parsedTypes []string
		if allowedTypes.Valid && allowedTypes.String != "" {
			_ = json.Unmarshal([]byte(allowedTypes.String), &parsedTypes)
		}
		endpoints = append(endpoints, map[string]any{
			"id": id, "endpoint_path": path, "table_name": tableName.String,
			"allowed_types": parsedTypes, "max_size_mb": maxSizeMB,
			"requires_auth": requiresAuth, "rate_limit": rateLimit,
			"owner_column": ownerColumn.String, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing upload endpoints: %w", err)
	}
	return tools.Ok(endpoints), nil
}

func epDeleteUpload(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_upload_endpoints", "upload endpoint", "manage_uploads", "delete", endpointPath)
}
