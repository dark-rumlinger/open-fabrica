package public

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openfabrica/security"
	"openfabrica/store"
)

// dynamicAPIQueryTimeout caps how long a single DB query in the public dynamic API
// can run. Bounds the damage from slow scans on large user-defined tables.
const dynamicAPIQueryTimeout = 15 * time.Second

// quoteColumnList takes the comma-separated column expression produced by
// visibleColumns/parseFieldsParam and returns it with each identifier wrapped
// in SQLite double-quotes. The sentinel "*" passes through unchanged because
// SQLite has no way to quote a star-projection. Any entry that fails identifier
// validation also passes through unchanged, the upstream code is expected to
// have rejected it already; we don't want to silently mangle something legal.
func quoteColumnList(cols string) string {
	if cols == "" || cols == "*" {
		return cols
	}
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		if name == "*" || security.ValidateColumnName(name) != nil {
			out = append(out, name)
			continue
		}
		out = append(out, security.QuoteIdent(name))
	}
	return strings.Join(out, ", ")
}

// reservedParams are query parameters with special meaning that are not column filters.
var reservedParams = map[string]bool{
	"limit": true, "offset": true, "sort": true, "order": true,
	"order_by": true, "direction": true, "order_direction": true, "order_dir": true, "page": true,
	"search": true, "stats": true, "q": true, "fields": true,
}

// errEndpointMisconfigured marks an endpoint row that exists but cannot be served
// safely. Callers answer 500 for it rather than the 404 of a missing endpoint.
var errEndpointMisconfigured = errors.New("endpoint misconfigured")

func (h *Handler) loadEndpoint(projectDB *sql.DB, path string) (*apiEndpoint, error) {
	row, err := store.LoadAPIEndpoint(projectDB, path)
	if err != nil {
		return nil, err
	}
	ep := apiEndpoint{
		ID:           row.ID,
		Path:         row.Path,
		TableName:    row.TableName,
		RequiresAuth: row.RequiresAuth,
		PublicRead:   row.PublicRead,
		PublicWrite:  row.PublicWrite,
		RequiredRole: row.RequiredRole.String,
		RateLimit:    row.RateLimit,
		CORSOrigins:  row.CORSOrigins,
		CORSMethods:  row.CORSMethods,
		CORSHeaders:  row.CORSHeaders,
		OwnerColumn:  row.OwnerColumn.String,
		OwnerScope:   row.OwnerScope,
		TenantColumn: row.TenantColumn.String,
		LookupColumn: row.LookupColumn.String,
		CacheTTL:     row.CacheTTL,
		ReadOnly:     row.ReadOnly,
	}
	// An owner or tenant column that fails the identifier check cannot be
	// interpolated, and dropping it would switch row-level security off. Refuse
	// to serve the endpoint instead.
	for name, col := range map[string]string{"owner_column": ep.OwnerColumn, "tenant_column": ep.TenantColumn} {
		if col == "" {
			continue
		}
		if err := security.ValidateColumnName(col); err != nil {
			slog.Error("public API endpoint has an invalid scope column; refusing to serve", "path", ep.Path, "field", name, "value", col, "error", err)
			return nil, fmt.Errorf("%w: %s %q", errEndpointMisconfigured, name, col)
		}
	}
	if ep.LookupColumn != "" {
		if err := security.ValidateColumnName(ep.LookupColumn); err != nil {
			slog.Error("public API endpoint has invalid lookup_column", "path", ep.Path, "lookup_column", ep.LookupColumn, "error", err)
			ep.LookupColumn = ""
		}
	}
	// Malformed stored JSON degrades to empty methods/columns rather than
	// failing the endpoint load.
	_ = json.Unmarshal([]byte(row.MethodsJSON), &ep.Methods)
	if row.PublicCols.Valid && row.PublicCols.String != "" {
		_ = json.Unmarshal([]byte(row.PublicCols.String), &ep.PublicColumns)
	}
	if row.WritableCols != "" {
		_ = json.Unmarshal([]byte(row.WritableCols), &ep.WritableColumns)
	}

	// Load secure columns for this table. Fail closed: if the lookup errors, the
	// endpoint must not serve rows with PASSWORD/ENCRYPTED columns left unstripped.
	secureCols, err := store.LoadSecureColumns(projectDB, ep.TableName)
	if err != nil {
		slog.Error("public API endpoint secure-column lookup failed; refusing to serve", "path", ep.Path, "table", ep.TableName, "error", err)
		return nil, fmt.Errorf("%w: load secure columns for %s: %v", errEndpointMisconfigured, ep.TableName, err)
	}
	ep.SecureCols = secureCols

	return &ep, nil
}

func methodAllowed(methods []string, method string) bool {
	for _, m := range methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}

// parseFilterParam extracts column name and SQL operator from a query param key.
// Supports: col (exact), col__like, col__has, col__gt, col__gte, col__lt,
// col__lte, col__ne.
//
// col__has is a membership test for comma-separated TEXT columns (e.g. tags):
// it matches when the value equals one whole comma-delimited token, not a
// substring. The caller builds the SQL; isToken signals that mode.
func parseFilterParam(key string) (column, op string, isLike, isToken, ok bool) {
	parts := strings.SplitN(key, "__", 2)
	column = parts[0]
	if len(parts) == 1 {
		return column, "=", false, false, true
	}
	switch parts[1] {
	case "like":
		return column, "LIKE", true, false, true
	case "has":
		return column, "", false, true, true
	case "gt":
		return column, ">", false, false, true
	case "gte":
		return column, ">=", false, false, true
	case "lt":
		return column, "<", false, false, true
	case "lte":
		return column, "<=", false, false, true
	case "ne":
		return column, "!=", false, false, true
	default:
		return "", "", false, false, false
	}
}

// tokenMembershipClause returns a SQL boolean expression (one LIKE, one ?
// placeholder) testing whether a comma-separated TEXT column contains a value as
// a whole token, not a substring: the column is comma-wrapped with comma-adjacent
// spaces collapsed, compared COLLATE NOCASE against tokenMembershipPattern.
// quotedCol must already be a validated, double-quoted identifier.
func tokenMembershipClause(quotedCol string) string {
	return "(',' || REPLACE(REPLACE(" + quotedCol + ", ', ', ','), ' ,', ',') || ',') LIKE ? COLLATE NOCASE"
}

// tokenMembershipPattern builds the LIKE pattern bound against the expression
// from tokenMembershipClause. The token is wrapped in commas so it only matches
// a whole element of the comma-separated list.
func tokenMembershipPattern(token string) string {
	return "%," + token + ",%"
}

// lookupSecureCol looks up col in secureCols case-insensitively because SQLite
// column identifiers are case-insensitive while incoming JSON/query/CSV keys
// may use arbitrary casing (e.g., "PASSWORD_HASH" vs "password_hash").
func lookupSecureCol(secureCols map[string]string, col string) (string, bool) {
	if len(secureCols) == 0 {
		return "", false
	}
	if kind, ok := secureCols[col]; ok {
		return kind, true
	}
	if kind, ok := secureCols[strings.ToLower(col)]; ok {
		return kind, true
	}
	for sc, kind := range secureCols {
		if strings.EqualFold(sc, col) {
			return kind, true
		}
	}
	return "", false
}

// deleteFold removes every key in m matching name case-insensitively.
func deleteFold(m map[string]any, name string) {
	if name == "" || len(m) == 0 {
		return
	}
	for k := range m {
		if strings.EqualFold(k, name) {
			delete(m, k)
		}
	}
}

// columnExists checks if a column name exists in the given table (case-insensitively,
// matching SQLite identifier resolution).
func columnExists(projectDB *sql.DB, physTable, colName string) bool {
	if !validTableName.MatchString(physTable) {
		return false
	}
	info, err := store.GetTableColumnInfo(projectDB, physTable)
	if err != nil {
		return false
	}
	for _, c := range info {
		if strings.EqualFold(c.Name, colName) {
			return true
		}
	}
	return false
}

// columnTypesOnce reads a table's columns once as name -> declared type, so an apiList
// request with k filters does ONE PRAGMA instead of ~2k per-column probes. Empty map when
// the table name is unsafe or introspection fails, so every membership/type check against it
// then reports "absent", exactly as the per-column helpers did on the same failure. The type
// is used to coerce filter values to the storage class SQLite holds (see coerceFilterValue).
func columnTypesOnce(projectDB *sql.DB, physTable string) map[string]string {
	if !validTableName.MatchString(physTable) {
		return map[string]string{}
	}
	info, err := store.GetTableColumnInfo(projectDB, physTable)
	if err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(info))
	for _, c := range info {
		out[c.Name] = c.Type
	}
	return out
}

// coerceFilterValue maps a raw query-string filter value to the storage class
// SQLite holds for the column. The case that matters in practice: BOOLEAN
// columns store 1/0, so a filter like ?is_active=true must bind the integer 1,
// not the literal text "true" (a TEXT-vs-INTEGER comparison never matches, so
// the row silently disappears - a seeded/active row that "isn't there"). Any
// non-boolean column, or an unrecognised value, passes through unchanged.
func coerceFilterValue(colType, val string) any {
	if !strings.EqualFold(colType, "BOOLEAN") && !strings.EqualFold(colType, "BOOL") {
		return val
	}
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "true", "1", "yes", "on":
		return 1
	case "false", "0", "no", "off":
		return 0
	}
	return val
}

// getTextColumns returns all TEXT column names from a table, excluding secure columns.
func getTextColumns(projectDB *sql.DB, physTable string, ep *apiEndpoint) []string {
	if !validTableName.MatchString(physTable) {
		return nil
	}
	info, err := store.GetTableColumnInfo(projectDB, physTable)
	if err != nil {
		return nil
	}
	var cols []string
	for _, c := range info {
		if strings.EqualFold(c.Type, "TEXT") && c.Name != "id" && c.Name != "created_at" {
			if !columnReadable(ep, c.Name) {
				continue
			}
			cols = append(cols, c.Name)
		}
	}
	return cols
}

// rowIDColumn returns the column name and value to use for single-row lookups.
// Resolution order for non-numeric rowIDs:
//  1. ep.LookupColumn if the endpoint declared one explicitly.
//  2. "uuid" or "slug" if a column with that name exists on the table.
//  3. The sole non-"id" UNIQUE TEXT column on the table, if exactly one exists
//     (rescues a domain-named public identifier like rooms.code or posts.share_id).
//  4. Fallback to "id". The query will not match, but the table is queried
//     consistently and the 404 surfaces in the normal path.
//
// Numeric rowIDs always resolve to "id".
func rowIDColumn(projectDB *sql.DB, physTable, rowID string, ep *apiEndpoint) (col string, val any) {
	if _, err := strconv.ParseInt(rowID, 10, 64); err == nil {
		return "id", rowID
	}
	if !validTableName.MatchString(physTable) {
		return "id", rowID
	}

	// 1. Explicit endpoint configuration wins. The loader already validated the
	//    identifier, so it's safe to interpolate as a column name.
	if ep != nil && ep.LookupColumn != "" && columnExists(projectDB, physTable, ep.LookupColumn) {
		return ep.LookupColumn, rowID
	}

	// 2. Naming-convention shortcut: uuid / slug.
	for _, candidate := range []string{"uuid", "slug"} {
		if columnExists(projectDB, physTable, candidate) {
			return candidate, rowID
		}
	}

	// 3. Auto-detect: exactly one non-"id" UNIQUE TEXT column.
	if auto := autoLookupColumn(projectDB, physTable); auto != "" {
		return auto, rowID
	}

	return "id", rowID
}

// autoLookupColumn returns the name of the table's sole non-"id" UNIQUE TEXT
// column, or "" if there is zero or more than one such column. Used by
// rowIDColumn as a heuristic for resolving /api/<path>/<value> when the
// endpoint did not declare lookup_column.
//
// "UNIQUE TEXT" here means a TEXT column that is the only column of a UNIQUE
// index (single-column uniqueness - composite UNIQUE constraints don't apply).
// SQLite implements `column TEXT UNIQUE` by creating exactly such an index.
func autoLookupColumn(projectDB *sql.DB, physTable string) string {
	if !validTableName.MatchString(physTable) {
		return ""
	}
	return store.AutoLookupColumn(projectDB, physTable)
}

// columnReadable reports whether a client may filter, sort or aggregate on col.
// Secure (PASSWORD/ENCRYPTED) columns never; otherwise public_columns bounds it
// when configured, so a hidden column cannot be probed through a query parameter.
func columnReadable(ep *apiEndpoint, col string) bool {
	if _, ok := lookupSecureCol(ep.SecureCols, col); ok {
		return false
	}
	if len(ep.PublicColumns) == 0 || strings.EqualFold(col, "id") || strings.EqualFold(col, "created_at") {
		return true
	}
	for _, c := range ep.PublicColumns {
		if strings.EqualFold(c, col) {
			return true
		}
	}
	return false
}

// restrictWritable drops body keys outside the endpoint's writable_columns.
// A no-op when the endpoint declares none.
func restrictWritable(ep *apiEndpoint, body map[string]any) {
	if len(ep.WritableColumns) == 0 {
		return
	}
	allowed := make(map[string]bool, len(ep.WritableColumns))
	for _, c := range ep.WritableColumns {
		allowed[strings.ToLower(c)] = true
	}
	for col := range body {
		if !allowed[strings.ToLower(col)] {
			delete(body, col)
		}
	}
}

// visibleColumns returns a SQL column list based on the endpoint's public_columns
// and secure column configuration. Secure (PASSWORD and ENCRYPTED) columns are
// always excluded; the bare-* fallback relies on stripSecureCols to drop them.
func (h *Handler) visibleColumns(ep *apiEndpoint) string {
	if len(ep.PublicColumns) > 0 {
		var safe []string
		for _, col := range ep.PublicColumns {
			if _, ok := lookupSecureCol(ep.SecureCols, col); ok {
				continue
			}
			safe = append(safe, col)
		}
		if len(safe) == 0 {
			return "id"
		}
		return strings.Join(safe, ", ")
	}
	return "*"
}

// parseFieldsParam reads ?fields=col1,col2 from the request and returns a
// validated, safe column list. Returns "" if the param is absent or empty,
// which signals the caller to use the default visibleColumns() result.
// Security: validates each name, strips secure columns, intersects with
// public_columns if configured, and always includes "id".
func parseFieldsParam(r *http.Request, ep *apiEndpoint) string {
	raw := r.URL.Query().Get("fields")
	if raw == "" {
		return ""
	}

	var allowed map[string]bool
	if len(ep.PublicColumns) > 0 {
		allowed = make(map[string]bool, len(ep.PublicColumns))
		for _, col := range ep.PublicColumns {
			allowed[strings.ToLower(col)] = true
		}
	}

	hasID := false
	seen := make(map[string]bool)
	var cols []string
	for _, f := range strings.Split(raw, ",") {
		col := strings.TrimSpace(f)
		low := strings.ToLower(col)
		if col == "" || seen[low] {
			continue
		}
		if security.ValidateColumnName(col) != nil {
			continue
		}
		if _, ok := lookupSecureCol(ep.SecureCols, col); ok {
			continue
		}
		if allowed != nil && !allowed[low] {
			continue
		}
		seen[low] = true
		cols = append(cols, col)
		if low == "id" {
			hasID = true
		}
	}

	if len(cols) == 0 {
		return ""
	}
	if !hasID {
		cols = append([]string{"id"}, cols...)
	}
	return strings.Join(cols, ", ")
}

// stripSecureCols removes every secure column (PASSWORD "hash" and ENCRYPTED
// "encrypt") from visitor-facing API results. Neither belongs in a response: a
// hash is irreversible, and an ENCRYPTED column holds a server-side secret that
// the trigger-events contract and every other read path (agent reads, the tools
// path's stripAllSecureCols) also strip. This mirrors that policy on the public API.
func stripSecureCols(results []map[string]any, secureCols map[string]string) {
	if len(secureCols) == 0 {
		return
	}
	for _, row := range results {
		for k := range row {
			if _, ok := lookupSecureCol(secureCols, k); ok {
				delete(row, k)
			}
		}
	}
}
