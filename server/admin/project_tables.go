package admin

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	"openfabrica/internal/csvsafe"
	"openfabrica/security"
	"openfabrica/store"
)

// ProjectTablesHandler handles dynamic table browsing for project detail views.
type ProjectTablesHandler struct {
	deps *Deps
}

type dynamicTable struct {
	ID         int       `json:"id"`
	TableName  string    `json:"table_name"`
	SchemaDef  string    `json:"schema_def"`
	SecureCols string    `json:"secure_columns"`
	RowCount   int       `json:"row_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// List returns all dynamic tables for a project with row counts.
func (h *ProjectTablesHandler) List(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	rows, err := projectDB.Query(
		`SELECT id, table_name, schema_def, secure_columns, created_at
		 FROM of_dynamic_tables ORDER BY table_name ASC`,
	)
	if err != nil {
		writeJSON(w, http.StatusOK, []dynamicTable{})
		return
	}
	defer rows.Close()

	var tables []dynamicTable
	for rows.Next() {
		var t dynamicTable
		if err := rows.Scan(&t.ID, &t.TableName, &t.SchemaDef, &t.SecureCols, &t.CreatedAt); err != nil {
			continue
		}

		// Get row count for each table. Table names in of_dynamic_tables come
		// from validated insertions, but defend in depth: skip any row whose
		// stored name fails the identifier grammar rather than interpolating it.
		if security.ValidateTableName(t.TableName) != nil {
			tables = append(tables, t)
			continue
		}
		_ = projectDB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", security.QuoteIdent(t.TableName))).Scan(&t.RowCount) // best-effort count

		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusOK, []dynamicTable{})
		return
	}

	if tables == nil {
		tables = []dynamicTable{}
	}

	writeJSON(w, http.StatusOK, tables)
}

// Rows returns rows from a dynamic table with pagination.
// PASSWORD columns are stripped from the response. ENCRYPTED columns show a placeholder.
func (h *ProjectTablesHandler) Rows(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	quotedTable := security.QuoteIdent(tableName)

	// Verify the table exists in this project's database.
	var count int
	err := projectDB.QueryRow(
		"SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?",
		tableName,
	).Scan(&count)
	if err != nil || count == 0 {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	// Load secure columns to filter PASSWORD columns from results. The error is
	// deliberately ignored here, unlike the write paths (InsertRow/UpdateRow) and
	// the project-facing readers, which fail closed: this is the localhost admin
	// panel rendering the owner's own rows, and it can decrypt every stored secret
	// by design, so hiding a hash is display hygiene rather than a trust boundary.
	// Refusing to render the table on a transient read error would cost more than
	// it protects. Do not copy this idiom to a path that leaves the control plane.
	secureCols, _ := store.LoadSecureColumns(projectDB.Reader(), tableName)
	hashCols := map[string]bool{}
	for col, kind := range secureCols {
		if kind == "hash" {
			hashCols[col] = true
		}
	}

	// Introspect the real columns so sort and search are validated against the
	// actual schema and never against raw query input. The PRAGMA cursor is drained
	// and closed before the data query runs on the same handle.
	tableCols := tableColumns(projectDB, quotedTable)

	// Optional ?sort=&dir= (validated column, asc/desc) and ?q= (substring search
	// across visible non-secret columns). Every identifier is checked against
	// tableCols and quoted; every value is a bound parameter.
	orderSQL := rowOrderClause(r.URL.Query().Get("sort"), r.URL.Query().Get("dir"), tableCols, hashCols)
	whereSQL, whereArgs := rowFilterClause(r.URL.Query().Get("q"), tableCols, hashCols, secureCols)

	// Get total count (filtered to match the search when one is active).
	var total int
	_ = projectDB.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s%s", quotedTable, whereSQL), whereArgs...).Scan(&total) // best-effort count

	// Query rows.
	queryArgs := append(append([]any{}, whereArgs...), limit, offset)
	rows, err := projectDB.Query(
		fmt.Sprintf("SELECT * FROM %s%s %s LIMIT ? OFFSET ?", quotedTable, whereSQL, orderSQL),
		queryArgs...,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query table")
		return
	}
	defer rows.Close()

	allColumns, err := rows.Columns()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get columns")
		return
	}

	// Filter out PASSWORD columns from the visible column list.
	var visibleColumns []string
	var visibleIndices []int
	for i, col := range allColumns {
		if !hashCols[col] {
			visibleColumns = append(visibleColumns, col)
			visibleIndices = append(visibleIndices, i)
		}
	}

	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(allColumns))
		ptrs := make([]any, len(allColumns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
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
			// Show placeholder for encrypted columns.
			if secureCols[col] == "encrypt" {
				row[col] = "[encrypted]"
			}
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query table")
		return
	}

	if result == nil {
		result = []map[string]any{}
	}

	// Also return schema info for the frontend to build CRUD forms.
	var schemaDef string
	_ = projectDB.QueryRow(
		"SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?",
		tableName,
	).Scan(&schemaDef)

	// schema_def values may be plain strings ("TEXT") or constraint objects
	// ({"type":"TEXT","not_null":true,...}); normalize to plain uppercase type
	// strings for the frontend.
	var rawSchema map[string]any
	_ = json.Unmarshal([]byte(schemaDef), &rawSchema) // unparseable schema_def just yields an empty schema map
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

	writeJSON(w, http.StatusOK, map[string]any{
		"columns":        visibleColumns,
		"rows":           result,
		"total":          total,
		"schema":         schemaMap,
		"secure_columns": secureCols,
	})
}

// ExportCSV exports all rows of a table as CSV.
func (h *ProjectTablesHandler) ExportCSV(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	var count int
	err := projectDB.QueryRow("SELECT COUNT(*) FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&count)
	if err != nil || count == 0 {
		writeError(w, http.StatusNotFound, "table not found")
		return
	}

	// Error deliberately ignored: same control-plane reasoning as Rows above.
	secureCols, _ := store.LoadSecureColumns(projectDB.Reader(), tableName)
	hashCols := map[string]bool{}
	for col, kind := range secureCols {
		if kind == "hash" {
			hashCols[col] = true
		}
	}

	rows, err := projectDB.Query(fmt.Sprintf("SELECT * FROM %s ORDER BY id ASC LIMIT 5000", security.QuoteIdent(tableName)))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to query table")
		return
	}
	defer rows.Close()

	allColumns, _ := rows.Columns()

	startCSV(w, tableName)

	writer := csv.NewWriter(w)

	// Write header (skip password columns).
	var header []string
	var indices []int
	for i, col := range allColumns {
		if !hashCols[col] {
			header = append(header, col)
			indices = append(indices, i)
		}
	}
	_ = writer.Write(header) // best-effort; the CSV is already streaming to the client

	for rows.Next() {
		values := make([]any, len(allColumns))
		ptrs := make([]any, len(allColumns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		var record []string
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
		_ = writer.Write(record)
	}
	if err := rows.Err(); err != nil {
		h.deps.Logger.Error("table export: row iteration failed", "table", tableName, "error", err)
	}
	writer.Flush()
}

var validColName = security.IdentifierRegex
var allowedTypes = map[string]string{
	"TEXT": "TEXT", "INTEGER": "INTEGER", "REAL": "REAL", "BOOLEAN": "BOOLEAN",
	"PASSWORD": "TEXT", "ENCRYPTED": "TEXT",
}

type addColumnRequest struct {
	ColumnName string `json:"column_name"`
	ColumnType string `json:"column_type"`
	NotNull    bool   `json:"not_null"`
}

// AddColumn adds a new column to an existing dynamic table.
func (h *ProjectTablesHandler) AddColumn(w http.ResponseWriter, r *http.Request) {
	_, projectDB := requireProjectDB(w, r, h.deps.ProjectDBManager)
	if projectDB == nil {
		return
	}

	tableName := chi.URLParam(r, "tableName")
	if err := security.ValidateTableName(tableName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	if store.IsSystemTable(tableName) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	var req addColumnRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if !validColName.MatchString(req.ColumnName) {
		writeError(w, http.StatusBadRequest, "invalid column name")
		return
	}

	colType := strings.ToUpper(req.ColumnType)
	sqlType, ok := allowedTypes[colType]
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported column type: "+req.ColumnType)
		return
	}

	colDef := req.ColumnName + " " + sqlType
	if req.NotNull {
		colDef += " NOT NULL DEFAULT ''"
	}

	_, err := projectDB.ExecWrite(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", security.QuoteIdent(tableName), colDef))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to add column")
		return
	}

	// Track secure columns.
	secureKinds := map[string]string{"PASSWORD": "hash", "ENCRYPTED": "encrypt"}
	if kind, isSecure := secureKinds[colType]; isSecure {
		var secureJSON string
		_ = projectDB.QueryRow("SELECT secure_columns FROM of_dynamic_tables WHERE table_name = ?", tableName).Scan(&secureJSON) // scan failure leaves "", handled below
		var secureCols map[string]string
		if json.Unmarshal([]byte(secureJSON), &secureCols) != nil {
			secureCols = map[string]string{}
		}
		secureCols[req.ColumnName] = kind
		newJSON, _ := json.Marshal(secureCols)
		if _, err := projectDB.ExecWrite("UPDATE of_dynamic_tables SET secure_columns = ? WHERE table_name = ?", string(newJSON), tableName); err != nil {
			h.deps.Logger.Error("failed to record secure column", "table", tableName, "column", req.ColumnName, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"table": tableName, "column_added": req.ColumnName})
}

// utf8BOM is the byte-order mark Excel needs to read a CSV as UTF-8. Without it a
// spreadsheet opened by double-click shows every non-ASCII character (accents,
// Cyrillic, Chinese, Japanese, Arabic, Devanagari) as mojibake, which is how most
// people open an export.
const utf8BOM = "\xEF\xBB\xBF"

// startCSV sets the headers for a CSV download and writes the byte-order mark.
func startCSV(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.csv", name))
	_, _ = w.Write([]byte(utf8BOM)) // best-effort; the body that follows is already streaming
}

// tableColumns returns a dynamic table's column names in schema order, via
// PRAGMA table_info. The cursor is drained and closed here so it never overlaps
// the data query on the same connection. Used only to validate a requested sort
// column and to pick searchable columns, so a read error yields an empty set
// (no sort, no search) rather than a failure.
func tableColumns(projectDB *db.ProjectDB, quotedTable string) []string {
	rows, err := projectDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", quotedTable))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			continue
		}
		cols = append(cols, name)
	}
	if rows.Err() != nil {
		return nil
	}
	return cols
}

// rowOrderClause builds a validated ORDER BY. A ?sort naming a real, non-hashed
// column sorts by it (?dir=asc|desc, default desc); anything else falls back to
// newest-first by id, the order the rows browser has always used.
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

// rowFilterClause builds a parameterized substring filter across the table's
// visible columns. Password columns are never present in the response and
// encrypted columns store ciphertext, so both are skipped. Returns ("", nil) when
// q is empty or nothing is searchable, leaving the caller's query unchanged.
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

// escapeLike escapes the LIKE wildcards so a user's search text matches
// literally. Pairs with the "ESCAPE '\'" on each LIKE clause above.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
