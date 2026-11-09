package data

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"openfabrica/internal/csvsafe"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

func DataExportCSV(ctx *tools.ToolContext, a *SQLExportCSV) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.FailCode(tools.CodeMissingArg, "table_name is required"), nil
	}
	tableName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	limit := a.Limit
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}

	// Build column list.
	colStar := "*"
	if len(a.Columns) > 0 {
		var cols []string
		for _, c := range a.Columns {
			if col, ok := c.(string); ok && col != "" {
				if err := security.ValidateColumnName(col); err != nil {
					return tools.Fail("invalid column name %q: %v", col, err), nil
				}
				cols = append(cols, col)
			}
		}
		if len(cols) > 0 {
			colStar = strings.Join(cols, ", ")
		}
	}

	// Build optional WHERE clause (BuildWhereClause returns it prefixed with
	// " WHERE " already).
	query := fmt.Sprintf("SELECT %s FROM %s", colStar, tableName)
	var queryArgs []any
	if len(a.Filters) > 0 {
		where, wArgs, wErr := store.BuildWhereClause(a.Filters)
		if wErr != nil {
			return tools.Fail("%s", wErr.Error()), nil
		}
		query += where
		queryArgs = append(queryArgs, wArgs...)
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := ctx.DB.Query(query, queryArgs...)
	if err != nil {
		return tools.Fail("%s", "query failed: "+err.Error()), nil
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	if len(cols) == 0 {
		return tools.Fail("no columns in result"), nil
	}

	// Strip secure columns from output.
	secureCols, _ := loadSecureColumns(ctx, tableName)

	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)

	// Write header (excluding PASSWORD columns).
	var headerCols []string
	var includeCol []bool
	for _, col := range cols {
		if secureCols != nil && secureCols[col] == "hash" {
			includeCol = append(includeCol, false)
		} else {
			headerCols = append(headerCols, col)
			includeCol = append(includeCol, true)
		}
	}
	_ = writer.Write(headerCols) // in-memory buffer write cannot fail

	// Write data rows.
	rowCount := 0
	values := make([]any, len(cols))
	scanDest := make([]any, len(cols))
	for i := range values {
		scanDest[i] = &values[i]
	}

	for rows.Next() {
		if rows.Scan(scanDest...) != nil {
			continue
		}
		var record []string
		for i, v := range values {
			if !includeCol[i] {
				continue
			}
			record = append(record, csvsafe.Cell(fmt.Sprintf("%v", v)))
		}
		_ = writer.Write(record) // in-memory buffer write cannot fail
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return tools.Fail("%s", "query failed: "+err.Error()), nil
	}
	writer.Flush()

	out := map[string]any{
		"table": tableName,
		"rows":  rowCount,
	}
	// The CSV rides the result, which the transcript caps; clip on a row boundary and
	// say so, instead of letting the cap cut it mid-row with no flag.
	csvText := buf.String()
	if len(csvText) > exportCSVResultCap {
		cut := strings.LastIndexByte(csvText[:exportCSVResultCap], '\n')
		if cut < 0 {
			cut = exportCSVResultCap
		}
		csvText = csvText[:cut+1]
		out["truncated"] = true
		out["rows_in_csv"] = strings.Count(csvText, string('\n')) - 1
		out["note"] = "the CSV was clipped to fit the result; pass fewer columns or a smaller limit to see a complete slice"
	}
	out["csv"] = csvText
	return tools.Ok(out), nil
}

// exportCSVResultCap keeps an export's CSV inside manage_sql's MaxResult with room for
// the surrounding fields.
const exportCSVResultCap = 10000

func DataImportCSV(ctx *tools.ToolContext, a *SQLImportCSV) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.FailCode(tools.CodeMissingArg, "table_name is required"), nil
	}
	tableName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	if a.CSVData == "" {
		return tools.FailCode(tools.CodeMissingArg, "csv_data is required"), nil
	}

	reader := csv.NewReader(strings.NewReader(a.CSVData))
	records, err := reader.ReadAll()
	if err != nil {
		return tools.Fail("%s", "invalid CSV: "+err.Error()), nil
	}
	if len(records) < 2 {
		return tools.Fail("CSV must have at least a header row and one data row"), nil
	}

	headers := records[0]
	dataRows := records[1:]

	if len(dataRows) > 500 {
		return tools.Fail("too many rows: %d (max 500)", len(dataRows)), nil
	}

	// Validate column names.
	for _, col := range headers {
		if err := security.ValidateColumnName(col); err != nil {
			return tools.Fail("%s", "invalid column name: "+col), nil
		}
	}

	// Exclude 'id' column, auto-generated.
	colIndexes := make([]int, 0, len(headers))
	colNames := make([]string, 0, len(headers))
	for i, col := range headers {
		if strings.ToLower(col) == "id" {
			continue
		}
		colIndexes = append(colIndexes, i)
		colNames = append(colNames, col)
	}

	if len(colNames) == 0 {
		return tools.Fail("no valid columns found in CSV header"), nil
	}

	placeholders := strings.Repeat("?, ", len(colNames))
	placeholders = placeholders[:len(placeholders)-2] // trim trailing ", "
	verb := "INSERT"
	onConflict := a.OnConflict
	switch onConflict {
	case "skip":
		verb = "INSERT OR IGNORE"
	case "update":
		verb = "INSERT OR REPLACE"
	}
	insertSQL := fmt.Sprintf("%s INTO %s (%s) VALUES (%s)",
		verb, tableName, strings.Join(colNames, ", "), placeholders)

	// Load secure columns and validation rules before the transaction: both read
	// ctx.DB, the single-connection writer pool the transaction will hold.
	secureCols, _ := loadSecureColumns(ctx, tableName)
	validationRules := store.LoadValidationRules(ctx.DB, tableName)

	// Insert in a transaction.
	tx, err := ctx.DB.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	inserted, skipped := 0, 0
	for i, row := range dataRows {
		rowMap := make(map[string]any, len(colNames))
		values := make([]any, len(colNames))
		for j, idx := range colIndexes {
			var val any = ""
			if idx < len(row) {
				val = row[idx]
			}
			rowMap[colNames[j]] = val
			values[j] = val
		}
		if errs := store.ValidateRow(validationRules, rowMap); len(errs) > 0 {
			if onConflict == "skip" {
				skipped++
				continue
			}
			return tools.Fail("row %d rejected by the table's validation rules: %s - nothing was imported", i+1, store.ValidationMessage(errs)), nil
		}
		// A secure value that cannot be hashed or encrypted fails the import: storing
		// the raw value would put a plaintext password or secret in the table.
		for j, col := range colNames {
			if kind, ok := secureCols[col]; ok {
				processed, pErr := store.ProcessSecureValue(kind, values[j], ctx.Encryptor)
				if pErr != nil {
					return tools.Fail("row %d: cannot secure %s column %s: %v - nothing was imported", i+1, kind, col, pErr), nil
				}
				values[j] = processed
			}
		}
		if _, err := tx.Exec(insertSQL, values...); err != nil {
			return tools.Fail("row %d: %s - nothing was imported (on_conflict=skip ignores duplicate-key rows)", i+1, err.Error()), nil
		}
		inserted++
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	return tools.Ok(map[string]any{
		"table":    tableName,
		"inserted": inserted,
		"skipped":  skipped,
	}), nil
}

func DataImportJSON(ctx *tools.ToolContext, a *SQLImportJSON) (*tools.Result, error) {
	if a.TableName == "" {
		return tools.FailCode(tools.CodeMissingArg, "table_name is required"), nil
	}
	tableName, err := store.SanitizedTableName(a.TableName)
	if err != nil {
		return tools.Fail("%s", err.Error()), nil
	}

	jsonData := a.JSONData
	if jsonData == "" {
		return tools.FailCode(tools.CodeMissingArg, "json_data is required"), nil
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(jsonData), &rows); err != nil {
		return tools.Fail("%s", "invalid JSON: "+err.Error()), nil
	}
	if len(rows) == 0 {
		return tools.Fail("JSON array is empty"), nil
	}
	if len(rows) > 500 {
		return tools.Fail("too many rows: %d (max 500)", len(rows)), nil
	}

	// Each row inserts the columns IT carries (minus id): a column read only from the
	// first row dropped every later row's extra fields, and padding a missing one with
	// NULL would override the column's default.
	for i, row := range rows {
		for col := range row {
			if strings.ToLower(col) == "id" {
				continue
			}
			if err := security.ValidateColumnName(col); err != nil {
				return tools.Fail("row %d: invalid column name %q", i+1, col), nil
			}
		}
	}
	onConflict := a.OnConflict
	verb := "INSERT"
	switch onConflict {
	case "skip":
		verb = "INSERT OR IGNORE"
	case "update":
		verb = "INSERT OR REPLACE"
	}

	secureCols, _ := loadSecureColumns(ctx, tableName)
	// Load validation rules and secure columns before opening the transaction.
	// Both query ctx.DB, which is the single-connection writer pool; querying
	// it while the transaction below holds that connection would deadlock.
	validationRules := store.LoadValidationRules(ctx.DB, tableName)

	tx, err := ctx.DB.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	inserted := 0
	skipped := 0
	var importErrors []string

	for i, row := range rows {
		if valErrors := store.ValidateRow(validationRules, row); len(valErrors) > 0 {
			if onConflict == "skip" {
				importErrors = append(importErrors, fmt.Sprintf("row %d: %s", i+1, store.ValidationMessage(valErrors)))
				skipped++
				continue
			}
			return tools.Fail("row %d rejected by the table's validation rules: %s - nothing was imported", i+1, store.ValidationMessage(valErrors)), nil
		}

		cols := make([]string, 0, len(row))
		for col := range row {
			if strings.ToLower(col) != "id" {
				cols = append(cols, col)
			}
		}
		if len(cols) == 0 {
			skipped++
			continue
		}
		sort.Strings(cols)
		values := make([]any, len(cols))
		for j, col := range cols {
			val := row[col]
			if kind, ok := secureCols[col]; ok {
				processed, pErr := store.ProcessSecureValue(kind, val, ctx.Encryptor)
				if pErr != nil {
					return tools.Fail("row %d: cannot secure %s column %s: %v - nothing was imported", i+1, kind, col, pErr), nil
				}
				val = processed
			}
			values[j] = val
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
		insertSQL := fmt.Sprintf("%s INTO %s (%s) VALUES (%s)", verb, tableName, strings.Join(cols, ", "), placeholders)
		if _, err := tx.Exec(insertSQL, values...); err != nil {
			return tools.Fail("row %d: %s - nothing was imported (on_conflict=skip ignores duplicate-key rows)", i+1, err.Error()), nil
		}
		inserted++
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	data := map[string]any{
		"table":    tableName,
		"imported": inserted,
		"skipped":  skipped,
	}
	if len(importErrors) > 0 {
		if len(importErrors) > 10 {
			importErrors = importErrors[:10]
		}
		data["errors"] = importErrors
	}
	return tools.Ok(data), nil
}
