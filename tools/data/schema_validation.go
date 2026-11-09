package data

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"openfabrica/store"
	"openfabrica/tools"
)

// ValidationError is the store type, kept under its old name for callers here.
type ValidationError = store.ValidationError

// SchemaSetValidation sets or updates validation rules for a table's columns.
func SchemaSetValidation(ctx *tools.ToolContext, tableName string, rules map[string]any) (*tools.Result, error) {
	if len(rules) == 0 {
		return tools.Fail("validation is required, provide a map of column_name -> rules"), nil
	}

	// Merge per column into what is stored instead of overwriting the whole value: the
	// column also carries internal keys (create_vector_index's _embedding_config) that a
	// blind overwrite erased, breaking a later embed.
	var stored map[string]any
	var raw string
	_ = ctx.DB.QueryRow("SELECT COALESCE(validation,'') FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&raw)
	if raw == "" || json.Unmarshal([]byte(raw), &stored) != nil {
		stored = map[string]any{}
	}
	for col, r := range rules {
		stored[col] = r
	}
	rulesJSON, _ := json.Marshal(stored)
	result, err := ctx.DB.Exec("UPDATE of_dynamic_tables SET validation = ? WHERE table_name = ?", string(rulesJSON), tableName)
	if err != nil {
		return nil, fmt.Errorf("setting validation: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return tools.Fail("table '%s' not found", tableName), nil
	}
	return tools.Ok(map[string]any{
		"table_name": tableName,
		"validation": rules,
	}), nil
}

// checkRowValidation applies the table's set_validation rules to rows about to be
// written, through tx when one is open (the rules load must not wait on the writer pool
// a transaction holds). Returns a failure naming the row and every broken rule, or nil.
func checkRowValidation(ctx *tools.ToolContext, tx *sql.Tx, tableName string, rows []map[string]any) *tools.Result {
	var rules map[string]map[string]any
	if tx != nil {
		rules = store.LoadValidationRules(tx, tableName)
	} else {
		rules = store.LoadValidationRules(ctx.DB, tableName)
	}
	if len(rules) == 0 {
		return nil
	}
	for i, row := range rows {
		if errs := store.ValidateRow(rules, row); len(errs) > 0 {
			where := ""
			if len(rows) > 1 {
				where = fmt.Sprintf(" (row %d)", i)
			}
			return tools.FailCode("validation_failed", "%s rejected by its set_validation rules%s: %s", tableName, where, store.ValidationMessage(errs))
		}
	}
	return nil
}
