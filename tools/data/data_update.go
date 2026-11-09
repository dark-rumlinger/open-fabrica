package data

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// DataUpdate updates rows by id or filters. tx is the outer transaction when
// dispatched from manage_sql's transaction action, nil on a standalone call.
func DataUpdate(ctx *tools.ToolContext, a *DataActUpdate, tx *sql.Tx) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.Fail("update requires a table_name"), nil
	}
	if len(a.Data) == 0 {
		return tools.Fail(
			"update of %q requires a non-empty data object (the column->value changes to write)", a.TableName), nil
	}

	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	if r := checkRowValidation(ctx, tx, a.TableName, []map[string]any{a.Data}); r != nil {
		return r, nil
	}
	secureCols, _ := loadSecureColumnsTx(ctx, tx, a.TableName)

	n, err := runUpdate(ctx, txExec(ctx, tx), physicalName, a.Data, int64(a.ID), a.Filters, secureCols)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}
	out := map[string]any{"table": a.TableName, "updated": n, "rows_affected": n}
	if n == 0 {
		// A zero-row update is a success at the SQL level but almost always a wrong id
		// or filter; say so instead of a silent "done".
		out["note"] = "no row matched, so nothing changed - check the id or filters (manage_data query shows what exists)"
	}
	return tools.Ok(out), nil
}

// runUpdate builds and executes a single UPDATE statement scoped by either id
// (single-row by primary key; 0 = not provided) or filters (column-op-value
// list, same syntax as query/batch_delete). Exactly one is required; passing
// both or neither is an error so the caller can't silently update zero rows.
// exec is the DB or an active tx, so it composes with batch/transaction.
func runUpdate(ctx *tools.ToolContext, exec sqlExec, physicalName string, data map[string]any, id int64, filters []any, secureCols map[string]string) (int64, error) {
	hasID := id != 0
	hasFilters := len(filters) > 0

	if !hasID && !hasFilters {
		return 0, fmt.Errorf("either id or filters is required")
	}
	if hasID && hasFilters {
		return 0, fmt.Errorf("provide either id or filters, not both")
	}

	var setClauses []string
	var values []any

	for col, val := range data {
		if !store.ValidColumnName.MatchString(col) {
			return 0, fmt.Errorf("invalid column name: %s", col)
		}
		if secureCols != nil {
			if kind, isSecure := secureCols[col]; isSecure {
				processed, perr := store.ProcessSecureValue(kind, val, ctx.Encryptor)
				if perr != nil {
					return 0, fmt.Errorf("processing secure column %s: %w", col, perr)
				}
				val = processed
			}
		}
		setClauses = append(setClauses, fmt.Sprintf("%s = ?", col))
		values = append(values, val)
	}

	var whereClause string
	if hasID {
		whereClause = "id = ?"
		values = append(values, id)
	} else {
		clause, params, werr := store.BuildWhereClause(filters)
		if werr != nil {
			return 0, werr
		}
		if clause == "" {
			return 0, fmt.Errorf("no valid filters provided")
		}
		whereClause = strings.TrimPrefix(clause, " WHERE ")
		values = append(values, params...)
	}

	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		physicalName, strings.Join(setClauses, ", "), whereClause)

	res, err := exec.Exec(query, values...)
	if err != nil {
		return 0, fmt.Errorf("updating row: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DataBatchUpdate runs up to 100 update entries in one owned transaction.
// Entries stay raw maps: {id|filters, data} is the wire shape of the updates
// array itself, not a tool-call convention.
func DataBatchUpdate(ctx *tools.ToolContext, a *DataActBatchUpdate) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.Fail("table_name is required"), nil
	}

	if len(a.Updates) == 0 {
		return tools.Fail("updates parameter is required (array of {filters|id, data})"), nil
	}
	if len(a.Updates) > 100 {
		return tools.Fail("maximum 100 entries per batch_update"), nil
	}

	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	secureCols, _ := loadSecureColumns(ctx, a.TableName)

	tx, err := ctx.DB.Begin()
	if err != nil {
		return nil, fmt.Errorf("starting transaction: %w", err)
	}
	defer tx.Rollback()

	results := make([]map[string]any, 0, len(a.Updates))
	var totalAffected int64

	for i, entryRaw := range a.Updates {
		entry, ok := entryRaw.(map[string]any)
		if !ok {
			return tools.Fail("entry %d is not a valid object", i), nil
		}
		data, _ := entry["data"].(map[string]any)
		if len(data) == 0 {
			return tools.Fail("entry %d: data is required", i), nil
		}

		idFloat, _ := entry["id"].(float64)
		filters, _ := entry["filters"].([]any)
		n, uerr := runUpdate(ctx, tx, physicalName, data, int64(idFloat), filters, secureCols)
		if uerr != nil {
			return tools.Fail("entry %d: %v", i, uerr), nil
		}
		totalAffected += n
		results = append(results, map[string]any{"rows_affected": n})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing batch update: %w", err)
	}

	return tools.Ok(map[string]any{
		"table":               a.TableName,
		"entries":             len(results),
		"updated":             totalAffected,
		"total_rows_affected": totalAffected,
		"results":             results,
	}), nil
}
