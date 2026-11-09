package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"openfabrica/internal/endpoints"
	"openfabrica/tools"

	"openfabrica/security"
)

// CSV export and CSV/JSON import handlers for manage_export_endpoint and
// manage_import_endpoint. Schemas live in manage_export_endpoint.go and
// manage_import_endpoint.go.

// CSV export (xlsx not yet wired)

// apiCreateExport takes typed args straight from ExportCreate. The format
// column is fixed at "csv" (ExportCreate exposes no format arg), matching the
// prior map path where the absent "format" key fell back to the default.
func apiCreateExport(ctx *tools.ToolContext, rawPath, table string, publicColumns []any, requiresAuth bool, owner string) (*tools.Result, error) {
	path, errRes := tools.ValidateAndNormalizeEndpointPath(rawPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" || table == "" {
		return tools.Fail("endpoint_path and table_name are required"), nil
	}
	format := "csv"
	if owner != "" {
		if err := security.ValidateColumnName(owner); err != nil {
			return nil, fmt.Errorf("owner_column: %w", err)
		}
	}

	var pubColsJSON string
	if len(publicColumns) > 0 {
		data, _ := json.Marshal(publicColumns)
		pubColsJSON = string(data)
	}

	if err := ClaimEndpoint(ctx, endpoints.KindExport, path, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(`
		INSERT INTO of_export_endpoints (path, table_name, format, public_columns, requires_auth, owner_column)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  table_name = excluded.table_name,
		  format = excluded.format,
		  public_columns = excluded.public_columns,
		  requires_auth = excluded.requires_auth,
		  owner_column = excluded.owner_column`,
		path, table, format, tools.NullIfEmpty(pubColsJSON), requiresAuth, tools.NullIfEmpty(owner),
	)
	if err != nil {
		return nil, fmt.Errorf("create_export: %w", err)
	}
	data := map[string]any{
		"endpoint_path": path,
		"url":           "/api/" + path + "/export",
	}
	if !requiresAuth && owner == "" {
		data["warning"] = "This export needs no login: anyone can download every exposed column of the table. Re-run create with requires_auth=true (plus owner_column to scope rows per user) unless the data is meant to be public."
	}
	return tools.Ok(data), nil
}

func apiListExport(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(`SELECT id, path, table_name, format, public_columns, requires_auth,
	                                  owner_column, tenant_column, rate_limit, created_at
	                             FROM of_export_endpoints ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, rateLimit int
		var path, table, format, tenantCol string
		var publicCols, ownerCol sql.NullString
		var ra bool
		var created time.Time
		if err := rows.Scan(&id, &path, &table, &format, &publicCols, &ra, &ownerCol, &tenantCol, &rateLimit, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "endpoint_path": path, "table_name": table,
			"format": format, "public_columns": publicCols.String,
			"requires_auth": ra, "owner_column": ownerCol.String,
			"tenant_column": tenantCol, "rate_limit": rateLimit,
			"url": "/api/" + path + "/export", "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tools.Ok(out), nil
}

func apiDeleteExport(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_export_endpoints", "export endpoint", "manage_export_endpoint", "delete", endpointPath)
}

// CSV import

// apiCreateImport takes typed args straight from ImportCreate. All three of mode,
// unique_column and max_rows are honoured by the runtime (server/public/csv.go
// branches on mode == "upsert" && uniqueCol != ""), so they are settable here rather
// than pinned to their defaults - an idempotent re-runnable import was otherwise a
// built capability with no way to ask for it.
func apiCreateImport(ctx *tools.ToolContext, rawPath, table string, requiresAuth bool, mode, uniqueCol string, maxRows int) (*tools.Result, error) {
	path, errRes := tools.ValidateAndNormalizeEndpointPath(rawPath)
	if errRes != nil {
		return errRes, nil
	}
	if path == "" || table == "" {
		return tools.Fail("endpoint_path and table_name are required"), nil
	}
	switch mode {
	case "":
		mode = "insert"
	case "insert":
	case "upsert":
		if uniqueCol == "" {
			return tools.Fail("write_mode=upsert needs unique_column: without a column to match on there is nothing to update, so every row would insert"), nil
		}
	default:
		return tools.Fail("write_mode must be insert or upsert, got %q", mode), nil
	}
	if maxRows <= 0 {
		maxRows = 10000
	}

	if err := ClaimEndpoint(ctx, endpoints.KindImport, path, nil); err != nil {
		return tools.FailErr(err), nil
	}

	_, err := ctx.DB.Exec(`
		INSERT INTO of_import_endpoints (path, table_name, mode, unique_column, requires_auth, max_rows)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  table_name = excluded.table_name,
		  mode = excluded.mode,
		  unique_column = excluded.unique_column,
		  requires_auth = excluded.requires_auth,
		  max_rows = excluded.max_rows`,
		path, table, mode, tools.NullIfEmpty(uniqueCol), requiresAuth, maxRows,
	)
	if err != nil {
		return nil, fmt.Errorf("create_import: %w", err)
	}
	return tools.Ok(map[string]any{
		"endpoint_path": path,
		"url":           "/api/" + path + "/import",
	}), nil
}

func apiListImport(ctx *tools.ToolContext) (*tools.Result, error) {
	rows, err := ctx.DB.Query(`SELECT id, path, table_name, mode, unique_column, requires_auth,
	                                  max_rows, tenant_column, created_at
	                             FROM of_import_endpoints ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, maxRows int
		var path, table, mode, tenantCol string
		var uniqueCol sql.NullString
		var ra bool
		var created time.Time
		if err := rows.Scan(&id, &path, &table, &mode, &uniqueCol, &ra, &maxRows, &tenantCol, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "endpoint_path": path, "table_name": table,
			"mode": mode, "unique_column": uniqueCol.String,
			"requires_auth": ra, "max_rows": maxRows,
			"tenant_column": tenantCol,
			"url":           "/api/" + path + "/import", "created_at": created,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tools.Ok(out), nil
}

func apiDeleteImport(ctx *tools.ToolContext, endpointPath string) (*tools.Result, error) {
	return DeleteEndpointByPath(ctx, "of_import_endpoints", "import endpoint", "manage_import_endpoint", "delete", endpointPath)
}
