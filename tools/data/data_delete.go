package data

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// DataDelete removes a single row by id. tx is the outer transaction when
// dispatched from manage_sql's transaction action, nil on a standalone call.
func DataDelete(ctx *tools.ToolContext, a *DataActDelete, tx *sql.Tx) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.Fail("table_name is required"), nil
	}
	if a.ID == 0 {
		return tools.Fail("id is required (use batch_delete with filters to delete by criteria)"), nil
	}

	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	res, err := txExec(ctx, tx).Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", physicalName), int64(a.ID))
	if err != nil {
		return nil, fmt.Errorf("deleting row: %w", err)
	}

	n, _ := res.RowsAffected()
	if n == 0 {
		return tools.NotFound("row with id", fmt.Sprint(a.ID)), nil
	}

	tools.LogDestructiveAction(txAudit(ctx, tx), "manage_data", "delete",
		fmt.Sprintf("%s id=%d", a.TableName, int64(a.ID)))

	return tools.Ok(map[string]any{
		"deleted_id": int64(a.ID),
	}), nil
}

// DataBatchDelete removes every row matching the filters. tx is the outer
// transaction when dispatched from manage_sql's transaction action, nil on a
// standalone call.
func DataBatchDelete(ctx *tools.ToolContext, a *DataActBatchDelete, tx *sql.Tx) (*tools.Result, error) {
	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	filters := a.Filters
	if len(filters) == 0 {
		return tools.Fail("filters required for batch_delete (safety: cannot delete all rows without filters)"), nil
	}

	// Use the shared, fail-closed WHERE builder (the same one manage_data update
	// uses). Hand-rolling this loop and silently `continue`-ing past a malformed
	// filter would DROP that predicate and widen the DELETE - deleting more rows
	// than the caller constrained. BuildWhereClause instead returns an error on
	// any bad filter, so a typo'd column or unsupported op fails the whole delete
	// rather than quietly over-deleting.
	where, vals, werr := store.BuildWhereClause(filters)
	if werr != nil {
		return tools.Fail("%s", werr.Error()), nil
	}
	if strings.TrimSpace(where) == "" {
		return tools.Fail("no valid filters provided"), nil
	}

	query := fmt.Sprintf("DELETE FROM %s%s", physicalName, where)
	result, err := txExec(ctx, tx).Exec(query, vals...)
	if err != nil {
		return tools.Fail("batch delete error: %v", err), nil
	}
	affected, _ := result.RowsAffected()

	tools.LogDestructiveAction(txAudit(ctx, tx), "manage_data", "batch_delete", fmt.Sprintf("%s: %d rows", a.TableName, affected))

	return tools.Ok(map[string]any{
		"table":   a.TableName,
		"deleted": affected,
	}), nil
}
