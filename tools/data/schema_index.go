package data

import (
	"fmt"
	"strings"

	"openfabrica/internal/dbh"
	"openfabrica/security"
	"openfabrica/store"
	"openfabrica/tools"
)

// CreateFTSIndex creates an FTS5 virtual table (<tableName>_fts) and the
// insert/update/delete triggers that keep it in sync with the source table.
func CreateFTSIndex(db dbh.Handle, tableName string, columns []string) error {
	// Every column is interpolated directly into the FTS5 DDL and the sync
	// triggers below, so reject anything that isn't a plain identifier. After
	// this gate each name matches ^[a-zA-Z][a-zA-Z0-9_]*$ and is non-reserved,
	// which is injection-safe to interpolate unquoted (the stored DDL is parsed
	// back by getFTSColumns, so we deliberately keep the names bare).
	for _, c := range columns {
		if err := security.ValidateColumnName(c); err != nil {
			return fmt.Errorf("invalid FTS column: %w", err)
		}
	}
	ftsTable := tableName + "_fts"
	colList := strings.Join(columns, ", ")

	// Create FTS5 virtual table.
	ftsSQL := fmt.Sprintf(
		"CREATE VIRTUAL TABLE IF NOT EXISTS %s USING fts5(%s, content=%s, content_rowid=id)",
		ftsTable, colList, tableName,
	)
	if _, err := db.Exec(ftsSQL); err != nil {
		return fmt.Errorf("creating FTS5 table: %w", err)
	}

	// Build column references for triggers.
	var newCols, oldCols []string
	for _, c := range columns {
		newCols = append(newCols, "new."+c)
		oldCols = append(oldCols, "old."+c)
	}
	newColList := strings.Join(newCols, ", ")
	oldColList := strings.Join(oldCols, ", ")

	// AFTER INSERT trigger.
	_, err := db.Exec(fmt.Sprintf(
		`CREATE TRIGGER IF NOT EXISTS %s_ai AFTER INSERT ON %s BEGIN
			INSERT INTO %s(rowid, %s) VALUES (new.id, %s);
		END`,
		tableName, tableName, ftsTable, colList, newColList,
	))
	if err != nil {
		return fmt.Errorf("creating INSERT trigger: %w", err)
	}

	// AFTER DELETE trigger.
	_, err = db.Exec(fmt.Sprintf(
		`CREATE TRIGGER IF NOT EXISTS %s_ad AFTER DELETE ON %s BEGIN
			INSERT INTO %s(%s, rowid, %s) VALUES('delete', old.id, %s);
		END`,
		tableName, tableName, ftsTable, ftsTable, colList, oldColList,
	))
	if err != nil {
		return fmt.Errorf("creating DELETE trigger: %w", err)
	}

	// AFTER UPDATE trigger.
	_, err = db.Exec(fmt.Sprintf(
		`CREATE TRIGGER IF NOT EXISTS %s_au AFTER UPDATE ON %s BEGIN
			INSERT INTO %s(%s, rowid, %s) VALUES('delete', old.id, %s);
			INSERT INTO %s(rowid, %s) VALUES (new.id, %s);
		END`,
		tableName, tableName,
		ftsTable, ftsTable, colList, oldColList,
		ftsTable, colList, newColList,
	))
	if err != nil {
		return fmt.Errorf("creating UPDATE trigger: %w", err)
	}

	return nil
}

// getFTSColumns returns the indexed column names for a table's FTS5 index,
// or nil if no FTS table exists. Parses the stored CREATE VIRTUAL TABLE SQL.
func getFTSColumns(db dbh.Handle, tableName string) []string {
	ftsTable := tableName + "_fts"
	var sqlText string
	err := db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='table' AND name=?", ftsTable,
	).Scan(&sqlText)
	if err != nil || sqlText == "" {
		return nil
	}
	// SQL looks like: CREATE VIRTUAL TABLE t_fts USING fts5(col1, col2, content=t, content_rowid=id)
	// Extract the columns before the first "content=" or "content_rowid=" parameter.
	start := strings.Index(sqlText, "(")
	end := strings.LastIndex(sqlText, ")")
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	inner := sqlText[start+1 : end]
	var cols []string
	for _, part := range strings.Split(inner, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "content=") || strings.HasPrefix(part, "content_rowid=") {
			continue
		}
		if part != "" {
			cols = append(cols, part)
		}
	}
	return cols
}

func SchemaAddIndex(ctx *tools.ToolContext, tableName string, indexColumns []any, unique bool) (*tools.Result, error) {
	if tableName == "" {
		return nil, fmt.Errorf("table_name is required")
	}
	if !store.ValidTableName.MatchString(tableName) {
		return nil, fmt.Errorf("invalid table name: %q", tableName)
	}

	// Validate table exists in of_dynamic_tables.
	var count int
	if err := ctx.DB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&count); err != nil {
		return nil, fmt.Errorf("checking table %q: %w", tableName, err)
	}
	if count == 0 {
		return tools.FailCode(tools.CodeNotFound, "table %q not found - create it with manage_schema action=create_table first (list_tables shows what exists)", tableName), nil
	}

	// Parse index_columns. A non-identifier token (commonly a table-qualified name
	// like "pinned_messages.message_id") must be rejected by name rather than
	// dropped: dropping it empties the list and yields a misleading "index_columns
	// is required", sending the model hunting for an arg it actually supplied.
	// Keep the two cases separate and name the bad token with a fix.
	var columns []string
	var invalid []string
	for _, c := range indexColumns {
		s, ok := c.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		if !store.ValidColumnName.MatchString(s) {
			invalid = append(invalid, s)
			continue
		}
		columns = append(columns, s)
	}
	if len(invalid) > 0 {
		hint := ""
		if strings.Contains(invalid[0], ".") {
			base := invalid[0][strings.LastIndex(invalid[0], ".")+1:]
			hint = fmt.Sprintf(" Use the bare column name (%q), not a table-qualified one (%q).", base, invalid[0])
		}
		return tools.Fail(
			"index_columns contains invalid column name(s) %v.%s", invalid, hint), nil
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("index_columns is required (array of column names)")
	}

	// Pre-check that every requested column exists on the table, with a
	// did-you-mean, instead of surfacing SQLite's bare "no such column: X" - which
	// costs the model a whole describe_table round-trip just to learn the real
	// columns (e.g. an index on a column the table never got). Best-effort: an introspection miss falls through to the raw exec below.
	if actual := physicalColumnNames(ctx.DB, tableName); len(actual) > 0 {
		have := make(map[string]bool, len(actual))
		for _, a := range actual {
			have[strings.ToLower(a)] = true
		}
		var missing []string
		for _, col := range columns {
			if !have[strings.ToLower(col)] {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			msg := fmt.Sprintf("cannot index %s(%s): column(s) %v do not exist. The table's columns are: %s.",
				tableName, strings.Join(columns, ", "), missing, strings.Join(actual, ", "))
			for _, m := range missing {
				if s := store.SuggestName(m, actual, 2); len(s) > 0 {
					msg += fmt.Sprintf(" Did you mean %s for %q?", strings.Join(s, " or "), m)
				}
			}
			msg += " If the column belongs on the table, add it with manage_schema action=alter_table first."
			return tools.Fail("%s", msg), nil
		}
	}

	// A unique index gets its own name: sharing the plain index's name made
	// "IF NOT EXISTS" skip it when a non-unique index on the same columns already
	// existed, while the result still reported unique:true.
	prefix := "idx_"
	if unique {
		prefix = "uidx_"
	}
	indexName := prefix + tableName + "_" + strings.Join(columns, "_")
	colList := strings.Join(columns, ", ")

	createStmt := "CREATE INDEX"
	if unique {
		createStmt = "CREATE UNIQUE INDEX"
	}
	stmt := fmt.Sprintf("%s IF NOT EXISTS %s ON %s(%s)", createStmt, indexName, tableName, colList)

	if _, err := ctx.DB.Exec(stmt); err != nil {
		return nil, fmt.Errorf("create index failed: %w", err)
	}

	return tools.Ok(map[string]any{
		"index":   indexName,
		"table":   tableName,
		"columns": columns,
		"unique":  unique,
	}), nil
}

// physicalColumnNames returns the live column names of a dynamic table (including
// platform-auto columns like id/created_at that of_dynamic_tables.schema_def does
// not record), or nil on any introspection miss.
func physicalColumnNames(db dbh.Handle, table string) []string {
	physical, err := store.SanitizedTableName(table)
	if err != nil {
		return nil
	}
	cols, err := store.GetTableColumnInfo(db, physical)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Name)
	}
	return out
}
