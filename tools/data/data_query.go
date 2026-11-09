package data

import (
	"fmt"
	"strings"

	"openfabrica/store"
	"openfabrica/tools"
)

// dataReadError enriches a raw SQLite read failure with a pointer to
// describe_table when the cause is a schema mismatch (a filter / order_by /
// column naming something the table doesn't have), so the model fixes the call
// on the first bounce instead of parsing the bare driver text. Other errors keep
// the wrapped driver message, just prefixed with the table for context.
func dataReadError(tableName string, err error) error {
	if s := err.Error(); strings.Contains(s, "no such column") || strings.Contains(s, "no such table") {
		return fmt.Errorf("%s - check %q's columns/name with manage_schema action=describe_table", s, tableName)
	}
	return fmt.Errorf("reading %q: %w", tableName, err)
}

func DataQuery(ctx *tools.ToolContext, a *DataActQuery) (*tools.Result, error) {
	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	// Load secure columns to know which to strip from results.
	secureCols, _ := loadSecureColumns(ctx, a.TableName)

	query := fmt.Sprintf("SELECT * FROM %s", physicalName)
	var queryParams []any

	// Structured filters (replaces raw WHERE string)
	if len(a.Filters) > 0 {
		whereClause, params, err := store.BuildWhereClause(a.Filters)
		if err != nil {
			return tools.Fail("%s", err.Error()), nil
		}
		query += whereClause
		queryParams = params
	}

	// Structured order_by (replaces raw ORDER BY string)
	if a.OrderBy != nil {
		col, _ := a.OrderBy["column"].(string)
		dir, _ := a.OrderBy["direction"].(string)
		if col != "" {
			if !store.ValidColumnName.MatchString(col) {
				return tools.Fail("invalid order_by column: %s", col), nil
			}
			dir = strings.ToUpper(strings.TrimSpace(dir))
			if dir != "ASC" && dir != "DESC" {
				dir = "ASC"
			}
			query += fmt.Sprintf(" ORDER BY %s %s", col, dir)
		}
	}

	if a.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", a.Limit)
	} else {
		query += " LIMIT 50" // default cap to prevent unbounded results
	}

	rows, err := ctx.DB.Query(query, queryParams...)
	if err != nil {
		return nil, dataReadError(a.TableName, err)
	}
	defer rows.Close()

	results := ScanRowsMaps(rows)
	stripAllSecureCols(results, secureCols)
	if err := rows.Err(); err != nil {
		return nil, dataReadError(a.TableName, err)
	}

	return tools.Ok(results), nil
}

func DataCount(ctx *tools.ToolContext, a *DataActCount) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.Fail("table_name is required"), nil
	}

	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	query := fmt.Sprintf("SELECT COUNT(*) FROM %s", physicalName)
	var queryParams []any

	if len(a.Filters) > 0 {
		whereClause, params, err := store.BuildWhereClause(a.Filters)
		if err != nil {
			return tools.Fail("%s", err.Error()), nil
		}
		query += whereClause
		queryParams = params
	}

	var count int
	err = ctx.DB.QueryRow(query, queryParams...).Scan(&count)
	if err != nil {
		return nil, dataReadError(a.TableName, err)
	}

	return tools.Ok(map[string]any{
		"table": a.TableName,
		"count": count,
	}), nil
}

// allowedAggFuncs is the whitelist of SQL aggregate functions.
var allowedAggFuncs = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
}

func DataAggregate(ctx *tools.ToolContext, a *DataActAggregate) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.Fail("table_name is required"), nil
	}

	physicalName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	fn := strings.ToLower(strings.TrimSpace(a.Function))
	if !allowedAggFuncs[fn] {
		return tools.Fail("function is required: count, sum, avg, min, or max"), nil
	}

	col := a.Column
	if fn != "count" && col == "" {
		return tools.Fail("%s requires a column parameter", fn), nil
	}
	if col != "" && !store.ValidColumnName.MatchString(col) {
		return tools.Fail("invalid column name: %s", col), nil
	}

	// Build aggregate expression.
	aggExpr := "COUNT(*)"
	if fn != "count" {
		aggExpr = fmt.Sprintf("%s(%s)", strings.ToUpper(fn), col)
	} else if col != "" {
		aggExpr = fmt.Sprintf("COUNT(%s)", col)
	}

	// Parse group_by columns.
	var groupCols []string
	for _, g := range a.GroupBy {
		gc, _ := g.(string)
		if gc == "" || !store.ValidColumnName.MatchString(gc) {
			return tools.Fail("invalid group_by column: %v", g), nil
		}
		groupCols = append(groupCols, gc)
	}

	// Build query.
	var selectParts []string
	selectParts = append(selectParts, groupCols...)
	selectParts = append(selectParts, aggExpr+" AS result")

	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectParts, ", "), physicalName)
	var queryParams []any

	// Apply filters.
	if len(a.Filters) > 0 {
		whereClause, params, err := store.BuildWhereClause(a.Filters)
		if err != nil {
			return tools.Fail("%s", err.Error()), nil
		}
		query += whereClause
		queryParams = params
	}

	if len(groupCols) > 0 {
		query += " GROUP BY " + strings.Join(groupCols, ", ")
		query += " ORDER BY " + strings.Join(groupCols, ", ")
	}

	// Execute.
	if len(groupCols) == 0 {
		// Single result (no grouping).
		// Scan loosely: SUM/AVG/MIN/MAX over no rows (or all-NULL rows) is NULL, and
		// MIN/MAX over a TEXT column is a string - a float64 scan errored on both.
		var result any
		err := ctx.DB.QueryRow(query, queryParams...).Scan(&result)
		if err != nil {
			return nil, dataReadError(a.TableName, err)
		}
		if b, ok := result.([]byte); ok {
			result = string(b)
		}
		return tools.Ok(map[string]any{
			"table":    a.TableName,
			"function": fn,
			"result":   result,
		}), nil
	}

	// Grouped results.
	rows, err := ctx.DB.Query(query, queryParams...)
	if err != nil {
		return nil, dataReadError(a.TableName, err)
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		// Build scan targets: one per group_by column + the result.
		scanTargets := make([]any, len(groupCols)+1)
		values := make([]any, len(groupCols)+1)
		for i := range scanTargets {
			scanTargets[i] = &values[i]
		}

		if err := rows.Scan(scanTargets...); err != nil {
			continue
		}

		row := make(map[string]any)
		for i, gc := range groupCols {
			row[gc] = values[i]
		}
		row["result"] = values[len(groupCols)]
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, dataReadError(a.TableName, err)
	}

	return tools.Ok(map[string]any{
		"table":    a.TableName,
		"function": fn,
		"data":     results,
	}), nil
}

func DataSearch(ctx *tools.ToolContext, a *DataActSearch) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.Fail("search requires a table_name"), nil
	}
	if a.QueryText == "" {
		return tools.Fail(
			"search of %q requires query_text (the text to search for)", a.TableName), nil
	}

	// The FTS query has ONE implementation: searchQuery. It fails closed when the
	// secure-column projection is unreadable (never hands back unsanitized rows) and
	// distinguishes a DB fault from a missing index. Delegating here keeps
	// manage_data.search and the keyword index tool from drifting on either.
	return searchQuery(ctx, a.TableName, a.QueryText, a.Limit)
}
