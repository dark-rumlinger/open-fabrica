package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/internal/sqliteretry"
	"openfabrica/security"
)

// APIEndpointRow holds the raw database columns of an of_api_endpoints row.
type APIEndpointRow struct {
	ID           int
	Path         string
	TableName    string
	MethodsJSON  string
	PublicCols   sql.NullString
	WritableCols string
	RequiresAuth bool
	PublicRead   bool
	PublicWrite  bool
	RequiredRole sql.NullString
	RateLimit    int
	CORSOrigins  string
	CORSMethods  string
	CORSHeaders  string
	OwnerColumn  sql.NullString
	OwnerScope   string
	TenantColumn sql.NullString
	LookupColumn sql.NullString
	CacheTTL     int
	ReadOnly     bool
}

// LoadAPIEndpoint loads a single of_api_endpoints row by path.
func LoadAPIEndpoint(reader *sql.DB, path string) (*APIEndpointRow, error) {
	var r APIEndpointRow
	err := reader.QueryRow(
		"SELECT id, path, table_name, methods, public_columns, COALESCE(writable_columns,''), requires_auth, public_read, COALESCE(public_write,0), required_role, rate_limit, COALESCE(cors_origins,''), COALESCE(cors_methods,''), COALESCE(cors_headers,''), COALESCE(owner_column,''), COALESCE(owner_scope,''), COALESCE(tenant_column,''), COALESCE(lookup_column,''), COALESCE(cache_ttl,0), COALESCE(read_only,0) FROM of_api_endpoints WHERE path = ?",
		path,
	).Scan(
		&r.ID, &r.Path, &r.TableName, &r.MethodsJSON, &r.PublicCols, &r.WritableCols,
		&r.RequiresAuth, &r.PublicRead, &r.PublicWrite, &r.RequiredRole, &r.RateLimit,
		&r.CORSOrigins, &r.CORSMethods, &r.CORSHeaders, &r.OwnerColumn, &r.OwnerScope,
		&r.TenantColumn, &r.LookupColumn, &r.CacheTTL, &r.ReadOnly,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// LoadTableForeignKeys returns a map of local column name -> referenced table name
// from PRAGMA foreign_key_list for physTable.
func LoadTableForeignKeys(reader *sql.DB, physTable string) (map[string]string, error) {
	rows, err := reader.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", physTable))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	fkRef := make(map[string]string)
	for rows.Next() {
		var id, seq int
		var refTable, from, to, onUpdate, onDelete, match string
		if rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match) == nil {
			fkRef[from] = refTable
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return fkRef, nil
}

// AutoLookupColumn returns the unique TEXT column name for physTable if exactly
// one single-column UNIQUE index exists on a non-id TEXT column, or "".
func AutoLookupColumn(reader *sql.DB, physTable string) string {
	info, err := GetTableColumnInfo(reader, physTable)
	if err != nil {
		return ""
	}
	colTypes := make(map[string]string, len(info))
	for _, c := range info {
		colTypes[c.Name] = strings.ToUpper(strings.TrimSpace(c.Type))
	}

	idxRows, err := reader.Query("SELECT name, \"unique\" FROM pragma_index_list(?)", physTable)
	if err != nil {
		return ""
	}
	type idx struct {
		name   string
		unique int
	}
	var idxs []idx
	for idxRows.Next() {
		var i idx
		if idxRows.Scan(&i.name, &i.unique) == nil && i.unique == 1 {
			idxs = append(idxs, i)
		}
	}
	idxRows.Close()
	if idxRows.Err() != nil {
		return ""
	}

	var candidates []string
	for _, i := range idxs {
		colRows, err := reader.Query("SELECT name FROM pragma_index_info(?)", i.name)
		if err != nil {
			continue
		}
		var cols []string
		for colRows.Next() {
			var n string
			if colRows.Scan(&n) == nil {
				cols = append(cols, n)
			}
		}
		colRows.Close()
		if colRows.Err() != nil || len(cols) != 1 {
			continue
		}
		c := cols[0]
		if c == "id" {
			continue
		}
		if t := colTypes[c]; t == "TEXT" || t == "" {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

// DynamicListQuery describes a paginated SELECT against a dynamic table.
type DynamicListQuery struct {
	Table      string
	QuotedCols string
	WhereSQL   string
	OrderBySQL string
	Args       []any
	Limit      int
	Offset     int
}

// ListDynamicRows runs a paginated SELECT against a dynamic table.
func ListDynamicRows(ctx context.Context, reader *sql.DB, q DynamicListQuery) ([]map[string]any, error) {
	orderClause := ""
	if q.OrderBySQL != "" {
		orderClause = " ORDER BY " + q.OrderBySQL
	}
	query := fmt.Sprintf("SELECT %s FROM %s%s%s LIMIT ? OFFSET ?",
		q.QuotedCols, security.QuoteIdent(q.Table), q.WhereSQL, orderClause)
	args := append(append([]any{}, q.Args...), q.Limit, q.Offset)
	rows, err := reader.QueryContext(ctx, query, args...) // #nosec G701 -- callers validate/QuoteIdent identifiers and bind values as ?
	if err != nil {
		return nil, err
	}
	return ScanRowsToMaps(rows), nil
}

// DynamicRowLookup describes a single-row lookup with optional RLS clauses.
type DynamicRowLookup struct {
	Table      string
	QuotedCols string
	LookupCol  string
	LookupVal  any
	RLSClauses string
	RLSArgs    []any
}

// GetDynamicRow fetches rows matching LookupCol = LookupVal (plus any RLS predicates).
func GetDynamicRow(ctx context.Context, reader *sql.DB, q DynamicRowLookup) ([]map[string]any, error) {
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?%s",
		q.QuotedCols, security.QuoteIdent(q.Table), security.QuoteIdent(q.LookupCol), q.RLSClauses)
	args := make([]any, 0, 1+len(q.RLSArgs))
	args = append(args, q.LookupVal)
	args = append(args, q.RLSArgs...)
	rows, err := reader.QueryContext(ctx, query, args...) // #nosec G701 -- callers validate/QuoteIdent identifiers and bind values as ?
	if err != nil {
		return nil, err
	}
	return ScanRowsToMaps(rows), nil
}

// DynamicStatsQuery describes a validated aggregation query over a dynamic table.
type DynamicStatsQuery struct {
	Table        string
	AggExpr      string
	GroupCols    []string
	WhereClauses []string
	WhereArgs    []any
}

func buildDynamicStatsQuery(q DynamicStatsQuery) string {
	quotedGroupCols := make([]string, len(q.GroupCols))
	for i, gc := range q.GroupCols {
		quotedGroupCols[i] = security.QuoteIdent(gc)
	}
	selectParts := make([]string, 0, len(quotedGroupCols)+1)
	selectParts = append(selectParts, quotedGroupCols...)
	selectParts = append(selectParts, q.AggExpr+" AS result")

	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectParts, ", "), security.QuoteIdent(q.Table))
	if len(q.WhereClauses) > 0 {
		query += " WHERE " + strings.Join(q.WhereClauses, " AND ")
	}
	if len(quotedGroupCols) > 0 {
		groupList := strings.Join(quotedGroupCols, ", ")
		query += " GROUP BY " + groupList + " ORDER BY " + groupList
	}
	return query
}

// QueryDynamicStatsScalar runs a parameterized scalar float64 aggregate query.
func QueryDynamicStatsScalar(ctx context.Context, reader *sql.DB, q DynamicStatsQuery) (float64, error) {
	var result float64
	err := reader.QueryRowContext(ctx, buildDynamicStatsQuery(q), q.WhereArgs...).Scan(&result) // #nosec G701 -- callers validate/QuoteIdent identifiers and bind values as ?
	return result, err
}

// QueryDynamicStatsGrouped runs a grouped aggregate query and returns one map per
// group with each group column name plus "result".
func QueryDynamicStatsGrouped(ctx context.Context, reader *sql.DB, q DynamicStatsQuery) ([]map[string]any, error) {
	rows, err := reader.QueryContext(ctx, buildDynamicStatsQuery(q), q.WhereArgs...) // #nosec G701 -- callers validate/QuoteIdent identifiers and bind values as ?
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		scanTargets := make([]any, len(q.GroupCols)+1)
		values := make([]any, len(q.GroupCols)+1)
		for i := range scanTargets {
			scanTargets[i] = &values[i]
		}
		if err := rows.Scan(scanTargets...); err != nil {
			continue
		}
		row := make(map[string]any)
		for i, gc := range q.GroupCols {
			if b, ok := values[i].([]byte); ok {
				row[gc] = string(b)
			} else {
				row[gc] = values[i]
			}
		}
		row["result"] = values[len(q.GroupCols)]
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// InsertDynamicRow inserts a row into a dynamic table with sqlite busy retry.
func InsertDynamicRow(writer *sql.DB, table string, columns []string, values []any) (sql.Result, error) {
	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = security.QuoteIdent(c)
		placeholders[i] = "?"
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		security.QuoteIdent(table), strings.Join(quoted, ", "), strings.Join(placeholders, ", "))
	return sqliteretry.Exec(writer, query, values...)
}

// DynamicUpdateSpec describes an UPDATE on a dynamic table with optional RLS clauses.
type DynamicUpdateSpec struct {
	Table      string
	Columns    []string
	Values     []any
	LookupCol  string
	LookupVal  any
	RLSClauses string
	RLSArgs    []any
}

// UpdateDynamicRow updates a row matching LookupCol = LookupVal (plus any RLS predicates).
func UpdateDynamicRow(writer *sql.DB, s DynamicUpdateSpec) (sql.Result, error) {
	setClauses := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		setClauses[i] = security.QuoteIdent(c) + " = ?"
	}
	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s = ?%s",
		security.QuoteIdent(s.Table), strings.Join(setClauses, ", "), security.QuoteIdent(s.LookupCol), s.RLSClauses)
	args := make([]any, 0, len(s.Values)+1+len(s.RLSArgs))
	args = append(args, s.Values...)
	args = append(args, s.LookupVal)
	args = append(args, s.RLSArgs...)
	return sqliteretry.Exec(writer, query, args...)
}

// DynamicDeleteSpec describes a DELETE on a dynamic table with optional RLS clauses.
type DynamicDeleteSpec struct {
	Table      string
	LookupCol  string
	LookupVal  any
	RLSClauses string
	RLSArgs    []any
}

// DeleteDynamicRow deletes a row matching LookupCol = LookupVal (plus any RLS predicates).
func DeleteDynamicRow(writer *sql.DB, s DynamicDeleteSpec) (sql.Result, error) {
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = ?%s",
		security.QuoteIdent(s.Table), security.QuoteIdent(s.LookupCol), s.RLSClauses)
	args := make([]any, 0, 1+len(s.RLSArgs))
	args = append(args, s.LookupVal)
	args = append(args, s.RLSArgs...)
	return sqliteretry.Exec(writer, query, args...)
}

// ScanRowsToMaps converts sql.Rows into a slice of maps and closes rows.
func ScanRowsToMaps(rows *sql.Rows) []map[string]any {
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil
	}

	results := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			continue
		}
		row := make(map[string]any)
		for i, col := range columns {
			val := values[i]
			if b, ok := val.([]byte); ok {
				row[col] = string(b)
			} else if f, ok := val.(float64); ok && f == float64(int64(f)) {
				row[col] = int64(f)
			} else {
				row[col] = val
			}
		}
		results = append(results, row)
	}
	return results
}

// ResolveSpineEndpointKind returns the endpoint kind that owns (path, sub_segment)
// for method in of_endpoints, or "" when none does.
func ResolveSpineEndpointKind(reader *sql.DB, path, subSegment, method string) string {
	var kind string
	err := reader.QueryRow(
		"SELECT kind FROM of_endpoints WHERE path = ? AND sub_segment = ? AND (method = ? OR method = '*') LIMIT 1",
		path, subSegment, strings.ToUpper(method),
	).Scan(&kind)
	if err != nil {
		return ""
	}
	return kind
}

// SpinePathExists reports whether any kind owns a route at this URL for any method.
func SpinePathExists(reader *sql.DB, fullPath, endpointPath, rowID string) bool {
	var n int
	if rowID != "" {
		if err := reader.QueryRow(
			"SELECT COUNT(*) FROM of_endpoints WHERE path = ? AND sub_segment = ?",
			endpointPath, rowID,
		).Scan(&n); err == nil && n > 0 {
			return true
		}
	}
	if err := reader.QueryRow(
		"SELECT COUNT(*) FROM of_endpoints WHERE path = ? AND sub_segment = ''", fullPath,
	).Scan(&n); err == nil && n > 0 {
		return true
	}
	return false
}
