package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"openfabrica/db"
	"openfabrica/internal/csvsafe"
	"openfabrica/security"
)

// ErrTableNotFound is returned when a dynamic table is not registered in of_dynamic_tables.
var ErrTableNotFound = errors.New("table not found")

// DynamicTableInfo describes a registered dynamic table and its row count.
type DynamicTableInfo struct {
	ID         int       `json:"id"`
	TableName  string    `json:"table_name"`
	SchemaDef  string    `json:"schema_def"`
	SecureCols string    `json:"secure_columns"`
	RowCount   int       `json:"row_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// TableRowsPage holds a paginated slice of dynamic table rows and schema metadata.
type TableRowsPage struct {
	Columns       []string          `json:"columns"`
	Rows          []map[string]any  `json:"rows"`
	Total         int               `json:"total"`
	Schema        map[string]string `json:"schema"`
	SecureColumns map[string]string `json:"secure_columns"`
}

// ListDynamicTables returns all registered dynamic tables with best-effort row counts.
func ListDynamicTables(pdb *db.ProjectDB) []DynamicTableInfo {
	rows, err := pdb.Query(
		`SELECT id, table_name, schema_def, secure_columns, created_at
		 FROM of_dynamic_tables ORDER BY table_name ASC`,
	)
	if err != nil {
		return []DynamicTableInfo{}
	}
	defer rows.Close()

	tables := []DynamicTableInfo{}
	for rows.Next() {
		var t DynamicTableInfo
		if rows.Scan(&t.ID, &t.TableName, &t.SchemaDef, &t.SecureCols, &t.CreatedAt) != nil {
			continue
		}
		if security.ValidateTableName(t.TableName) == nil {
			_ = pdb.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", security.QuoteIdent(t.TableName))).Scan(&t.RowCount)
		}
		tables = append(tables, t)
	}
	if rows.Err() != nil {
		return []DynamicTableInfo{}
	}
	return tables
}

// DynamicTableExists reports whether tableName is registered in of_dynamic_tables.
func DynamicTableExists(pdb *db.ProjectDB, tableName string) bool {
	var count int
	err := pdb.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&count)
	return err == nil && count > 0
}

// QueryDynamicTableRows returns a filtered, sorted, paginated page of rows from tableName.
func QueryDynamicTableRows(pdb *db.ProjectDB, tableName, sortCol, sortDir, search string, limit, offset int) (*TableRowsPage, error) {
	if !DynamicTableExists(pdb, tableName) {
		return nil, ErrTableNotFound
	}
	quotedTable := security.QuoteIdent(tableName)

	secureCols, _ := LoadSecureColumns(pdb.Reader(), tableName)
	hashCols := map[string]bool{}
	for col, kind := range secureCols {
		if kind == "hash" {
			hashCols[col] = true
		}
	}

	tableCols := dynamicTableColumns(pdb, quotedTable)
	orderSQL := rowOrderClause(sortCol, sortDir, tableCols, hashCols)
	whereSQL, whereArgs := rowFilterClause(search, tableCols, hashCols, secureCols)

	var total int
	_ = pdb.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s%s", quotedTable, whereSQL), whereArgs...).Scan(&total)

	queryArgs := append(append([]any{}, whereArgs...), limit, offset)
	rows, err := pdb.Query(
		fmt.Sprintf("SELECT * FROM %s%s %s LIMIT ? OFFSET ?", quotedTable, whereSQL, orderSQL),
		queryArgs...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	allColumns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var visibleColumns []string
	var visibleIndices []int
	for i, col := range allColumns {
		if !hashCols[col] {
			visibleColumns = append(visibleColumns, col)
			visibleIndices = append(visibleIndices, i)
		}
	}

	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(allColumns))
		ptrs := make([]any, len(allColumns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		row := make(map[string]any)
		for _, idx := range visibleIndices {
			col := allColumns[idx]
			if b, ok := values[idx].([]byte); ok {
				row[col] = string(b)
			} else if f, ok := values[idx].(float64); ok && f == float64(int64(f)) {
				row[col] = int64(f)
			} else {
				row[col] = values[idx]
			}
			if secureCols[col] == "encrypt" {
				row[col] = "[encrypted]"
			}
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var schemaDef string
	_ = pdb.QueryRow("SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&schemaDef)
	var rawSchema map[string]any
	_ = json.Unmarshal([]byte(schemaDef), &rawSchema)
	schemaMap := make(map[string]string, len(rawSchema))
	for k, v := range rawSchema {
		switch t := v.(type) {
		case string:
			schemaMap[k] = strings.ToUpper(t)
		case map[string]any:
			if s, ok := t["type"].(string); ok {
				schemaMap[k] = strings.ToUpper(s)
			}
		}
	}

	return &TableRowsPage{
		Columns:       visibleColumns,
		Rows:          result,
		Total:         total,
		Schema:        schemaMap,
		SecureColumns: secureCols,
	}, nil
}

// ExportDynamicTableCSV reads up to 5000 rows from tableName and returns the
// CSV header (excluding PASSWORD columns) and sanitized records.
func ExportDynamicTableCSV(pdb *db.ProjectDB, tableName string) (header []string, records [][]string, err error) {
	if !DynamicTableExists(pdb, tableName) {
		return nil, nil, ErrTableNotFound
	}

	secureCols, _ := LoadSecureColumns(pdb.Reader(), tableName)
	hashCols := map[string]bool{}
	for col, kind := range secureCols {
		if kind == "hash" {
			hashCols[col] = true
		}
	}

	rows, err := pdb.Query(fmt.Sprintf("SELECT * FROM %s ORDER BY id ASC LIMIT 5000", security.QuoteIdent(tableName)))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	allColumns, _ := rows.Columns()
	var indices []int
	for i, col := range allColumns {
		if !hashCols[col] {
			header = append(header, col)
			indices = append(indices, i)
		}
	}

	for rows.Next() {
		values := make([]any, len(allColumns))
		ptrs := make([]any, len(allColumns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		record := make([]string, 0, len(indices))
		for _, idx := range indices {
			col := allColumns[idx]
			if secureCols[col] == "encrypt" {
				record = append(record, "[encrypted]")
			} else if values[idx] == nil {
				record = append(record, "")
			} else {
				record = append(record, csvsafe.Cell(fmt.Sprintf("%v", values[idx])))
			}
		}
		records = append(records, record)
	}
	return header, records, rows.Err()
}

// AddDynamicTableColumn alters tableName to add colName and updates secure_columns metadata if needed.
func AddDynamicTableColumn(pdb *db.ProjectDB, tableName, colName, colType, sqlType string, notNull bool) error {
	colDef := colName + " " + sqlType
	if notNull {
		colDef += " NOT NULL DEFAULT ''"
	}
	if _, err := pdb.ExecWrite(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", security.QuoteIdent(tableName), colDef)); err != nil {
		return err
	}

	secureKinds := map[string]string{"PASSWORD": "hash", "ENCRYPTED": "encrypt"}
	if kind, isSecure := secureKinds[colType]; isSecure {
		var secureJSON string
		_ = pdb.QueryRow("SELECT secure_columns FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&secureJSON)
		var secureCols map[string]string
		if json.Unmarshal([]byte(secureJSON), &secureCols) != nil {
			secureCols = map[string]string{}
		}
		secureCols[colName] = kind
		newJSON, _ := json.Marshal(secureCols)
		if _, err := pdb.ExecWrite("UPDATE of_dynamic_tables SET secure_columns = ? WHERE table_name = ?", string(newJSON), tableName); err != nil {
			slog.Error("failed to record secure column", "table", tableName, "column", colName, "error", err)
		}
	}
	return nil
}

// InsertDynamicTableRow inserts one validated row into tableName and returns its row ID.
func InsertDynamicTableRow(pdb *db.ProjectDB, tableName string, quotedCols []string, values []any) (int64, error) {
	placeholders := make([]string, len(quotedCols))
	for i := range quotedCols {
		placeholders[i] = "?"
	}
	query := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
		security.QuoteIdent(tableName), strings.Join(quotedCols, ", "), strings.Join(placeholders, ", "))
	res, err := pdb.ExecWrite(query, values...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateDynamicTableRow updates rowID in tableName with the given SET clauses.
func UpdateDynamicTableRow(pdb *db.ProjectDB, tableName string, rowID int, setClauses []string, values []any) error {
	args := append(append([]any{}, values...), rowID)
	query := fmt.Sprintf(`UPDATE %s SET %s WHERE id = ?`,
		security.QuoteIdent(tableName), strings.Join(setClauses, ", "))
	_, err := pdb.ExecWrite(query, args...)
	return err
}

// DeleteDynamicTableRow deletes rowID from tableName.
func DeleteDynamicTableRow(pdb *db.ProjectDB, tableName string, rowID int) error {
	_, err := pdb.ExecWrite(fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, security.QuoteIdent(tableName)), rowID)
	return err
}

func dynamicTableColumns(pdb *db.ProjectDB, quotedTable string) []string {
	rows, err := pdb.Query(fmt.Sprintf("PRAGMA table_info(%s)", quotedTable))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil {
			cols = append(cols, name)
		}
	}
	if rows.Err() != nil {
		return nil
	}
	return cols
}

func rowOrderClause(sort, dir string, cols []string, hashCols map[string]bool) string {
	if sort != "" && !hashCols[sort] && slices.Contains(cols, sort) {
		direction := "DESC"
		if strings.EqualFold(dir, "asc") {
			direction = "ASC"
		}
		return "ORDER BY " + security.QuoteIdent(sort) + " " + direction
	}
	return "ORDER BY id DESC"
}

func rowFilterClause(q string, cols []string, hashCols map[string]bool, secureCols map[string]string) (string, []any) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil
	}
	like := "%" + escapeLike(q) + "%"
	var clauses []string
	var args []any
	for _, col := range cols {
		if hashCols[col] || secureCols[col] == "encrypt" {
			continue
		}
		clauses = append(clauses, security.QuoteIdent(col)+` LIKE ? ESCAPE '\'`)
		args = append(args, like)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE (" + strings.Join(clauses, " OR ") + ")", args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
