// Package data implements the data CRUD tool surface. Operations are split
// across sibling files by family: data_query.go (read), data_insert.go
// (insert/upsert), data_update.go (update/batch_update), data_delete.go
// (delete/batch_delete), data_exec.go (raw SELECT), data_io.go (CSV/JSON
// import & export). This file owns only the cross-family helpers:
// transaction routing, row scanning, secure-column stripping, and result
// summaries used by the tool registry.

package data

import (
	"database/sql"
	"fmt"

	"openfabrica/store"
	"openfabrica/tools"
)

// sqlExec is the common Exec surface shared by *sql.DB and *sql.Tx, letting
// runUpdate and bulk-insert run inside or outside a transaction.
type sqlExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// txExec is the canonical executor lookup for any data-mutation handler that
// must compose into an outer transaction: the tx the transaction action passed
// down, or the request-scoped DB when nil so single-call usage is unchanged.
func txExec(ctx *tools.ToolContext, tx *sql.Tx) sqlExec {
	if tx != nil {
		return tx
	}
	return ctx.DB
}

// txAudit returns the executor for a destructive-action audit write: the active
// transaction when one is present, otherwise the request DB. The audit row must
// run on the same connection as the mutation, since the writer pool is a single
// connection and a separate ctx.DB query would deadlock against an open tx.
func txAudit(ctx *tools.ToolContext, tx *sql.Tx) tools.AuditExecutor {
	if tx != nil {
		return tx
	}
	return ctx.DB
}

// ScanRowsMaps scans rows into a slice of maps (generic column scanning).
func ScanRowsMaps(rows *sql.Rows) []map[string]any {
	cols, err := rows.Columns()
	if err != nil {
		return nil
	}
	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		row := make(map[string]any)
		for i, col := range cols {
			if f, ok := values[i].(float64); ok && f == float64(int64(f)) {
				row[col] = int64(f)
			} else {
				row[col] = values[i]
			}
		}
		results = append(results, row)
	}
	return results
}

// stripAllSecureCols removes EVERY secure column (PASSWORD "hash" and ENCRYPTED
// "encrypt") from result maps. Neither is useful to the model: a hash cannot be
// reversed, and the stored ciphertext is noise that only spends context. Every read
// path that returns rows to the model goes through this, searches included. Contrast
// stripPasswordHashCols (server/public), which strips only hashes on the visitor API.
func stripAllSecureCols(results []map[string]any, secureCols map[string]string) {
	if len(secureCols) == 0 {
		return
	}
	for _, row := range results {
		for col := range secureCols {
			delete(row, col)
		}
	}
}

// loadSecureColumnsOrFail loads a table's secure columns for a read that returns rows
// to the model. An unreadable projection means the secret columns are unknown, so the
// read refuses instead of handing back rows it could not sanitize.
func loadSecureColumnsOrFail(ctx *tools.ToolContext, tableName string) (map[string]string, *tools.Result) {
	secureCols, err := store.LoadSecureColumns(ctx.DB, tableName)
	if err != nil {
		return nil, tools.Fail("secure-column check failed for '%s', refusing to return unsanitized rows: %v", tableName, err)
	}
	return secureCols, nil
}

func DataSummarize(result string) string {
	return tools.SummarizeFrame(result, "Queried: %d rows returned", func(dataMap map[string]any) string {
		// Dedicated branches for data-mutation results so compacted history stays accurate.
		if inserted, ok := dataMap["inserted"].(float64); ok {
			table, _ := dataMap["table"].(string)
			return fmt.Sprintf(`{"success":true,"summary":"Inserted %d rows into %s"}`, int(inserted), table)
		}
		if updated, ok := dataMap["updated"].(float64); ok {
			table, _ := dataMap["table"].(string)
			return fmt.Sprintf(`{"success":true,"summary":"Updated %d rows in %s"}`, int(updated), table)
		}
		if _, ok := dataMap["upserted"]; ok {
			table, _ := dataMap["table"].(string)
			return fmt.Sprintf(`{"success":true,"summary":"Upserted row in %s"}`, table)
		}
		return ""
	})
}

// MergedSummarize delegates to SchemaSummarize or DataSummarize based on the
// result shape. Schema results have a "table" key; data mutations also have a
// "table" key but are distinguished by "inserted"/"updated"/"upserted" fields.
func MergedSummarize(result string) string {
	r, dataMap, _, ok := tools.ParseSummaryResult(result)
	if ok && r.Success {
		if tableName, _ := dataMap["table"].(string); tableName != "" {
			// Data mutations include "inserted"/"updated"/"upserted" alongside "table".
			// Route those to DataSummarize so they don't appear as schema operations
			// in the compacted history.
			_, isInsert := dataMap["inserted"]
			_, isUpdated := dataMap["updated"]
			_, isUpsert := dataMap["upserted"]
			if !isInsert && !isUpdated && !isUpsert {
				return SchemaSummarize(result)
			}
		}
	}
	return DataSummarize(result)
}
