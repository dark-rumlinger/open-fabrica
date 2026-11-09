package public

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openfabrica/db/models"
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

func (h *Handler) loadEndpoint(projectDB *sql.DB, path string) (*apiEndpoint, error) {
	var ep apiEndpoint
	var methodsJSON string
	var publicColsJSON, requiredRole, ownerColumn, tenantColumn, lookupColumn sql.NullString
	var writableColsJSON string
	var corsOrigins, corsMethods, corsHeaders sql.NullString
	err := projectDB.QueryRow(
		"SELECT id, path, table_name, methods, public_columns, COALESCE(writable_columns,''), requires_auth, public_read, COALESCE(public_write,0), required_role, rate_limit, COALESCE(cors_origins,''), COALESCE(cors_methods,''), COALESCE(cors_headers,''), COALESCE(owner_column,''), COALESCE(owner_scope,''), COALESCE(tenant_column,''), COALESCE(lookup_column,''), COALESCE(cache_ttl,0), COALESCE(read_only,0) FROM of_api_endpoints WHERE path = ?",
		path,
	).Scan(&ep.ID, &ep.Path, &ep.TableName, &methodsJSON, &publicColsJSON, &writableColsJSON, &ep.RequiresAuth, &ep.PublicRead, &ep.PublicWrite, &requiredRole, &ep.RateLimit, &corsOrigins, &corsMethods, &corsHeaders, &ownerColumn, &ep.OwnerScope, &tenantColumn, &lookupColumn, &ep.CacheTTL, &ep.ReadOnly)
	if err != nil {
		return nil, err
	}

	ep.RequiredRole = requiredRole.String
	ep.OwnerColumn = ownerColumn.String
	ep.TenantColumn = tenantColumn.String
	// Defensive: refuse to interpolate an owner_column that doesn't pass the
	// canonical identifier check, even though writers validate at insert time.
	// Treat a bad value as no RLS so the request fails closed on access checks
	// rather than producing a SQL injection vector.
	if ep.OwnerColumn != "" {
		if err := security.ValidateColumnName(ep.OwnerColumn); err != nil {
			slog.Error("public API endpoint has invalid owner_column", "path", ep.Path, "owner_column", ep.OwnerColumn, "error", err)
			ep.OwnerColumn = ""
		}
	}
	if ep.TenantColumn != "" {
		if err := security.ValidateColumnName(ep.TenantColumn); err != nil {
			slog.Error("public API endpoint has invalid tenant_column", "path", ep.Path, "tenant_column", ep.TenantColumn, "error", err)
			ep.TenantColumn = ""
		}
	}
	ep.LookupColumn = lookupColumn.String
	if ep.LookupColumn != "" {
		if err := security.ValidateColumnName(ep.LookupColumn); err != nil {
			slog.Error("public API endpoint has invalid lookup_column", "path", ep.Path, "lookup_column", ep.LookupColumn, "error", err)
			ep.LookupColumn = ""
		}
	}
	ep.CORSOrigins = corsOrigins.String
	ep.CORSMethods = corsMethods.String
	ep.CORSHeaders = corsHeaders.String
	// Malformed stored JSON degrades to empty methods/columns rather than
	// failing the endpoint load.
	_ = json.Unmarshal([]byte(methodsJSON), &ep.Methods)
	if publicColsJSON.Valid && publicColsJSON.String != "" {
		_ = json.Unmarshal([]byte(publicColsJSON.String), &ep.PublicColumns)
	}
	if writableColsJSON != "" {
		_ = json.Unmarshal([]byte(writableColsJSON), &ep.WritableColumns)
	}

	// Load secure columns for this table. Fail closed: if the lookup errors, the
	// endpoint must not serve rows with PASSWORD/ENCRYPTED columns left unstripped.
	secureCols, err := store.LoadSecureColumns(projectDB, ep.TableName)
	if err != nil {
		slog.Error("public API endpoint secure-column lookup failed; refusing to serve", "path", ep.Path, "table", ep.TableName, "error", err)
		return nil, fmt.Errorf("load secure columns for %s: %w", ep.TableName, err)
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

func (h *Handler) validateProjectToken(projectID int, token string) bool {
	if token == "" || h.deps.Encryptor == nil {
		return false
	}
	project, err := models.GetProjectByID(h.deps.DB.DB, projectID)
	if err != nil {
		return false
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(project.Config), &cfg); err != nil {
		return false
	}
	// The project API key grants owner-level access (it bypasses row-level
	// security), so it is held encrypted at rest in the project config under
	// "api_key_encrypted", the same way provider keys are stored. A plaintext
	// "api_key" is deliberately NOT honored: it would be a credential readable by
	// anyone who can read the global database, and an injected one must not be
	// able to bypass RLS.
	enc, _ := cfg["api_key_encrypted"].(string)
	if enc == "" {
		return false
	}
	apiKey, decErr := h.deps.Encryptor.Decrypt(enc)
	if decErr != nil || apiKey == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(apiKey), []byte(token)) == 1
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

// columnExists checks if a column name exists in the given table.
func columnExists(projectDB *sql.DB, physTable, colName string) bool {
	if !validTableName.MatchString(physTable) {
		return false
	}
	rows, err := projectDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", physTable))
	if err != nil {
		return false
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		if rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk) != nil {
			continue
		}
		if name == colName {
			found = true
		}
	}
	// A failed iteration is handled like a failed query: column reported absent.
	if rows.Err() != nil {
		return false
	}
	return found
}

// columnType returns the declared type of a column (e.g. "TEXT", "INTEGER",
// "BOOLEAN") exactly as recorded in the table schema, or "" when the column is
// absent. Used to coerce query-string filter values to the storage class SQLite
// actually holds (see coerceFilterValue).
func columnType(projectDB *sql.DB, physTable, colName string) string {
	if !validTableName.MatchString(physTable) {
		return ""
	}
	rows, err := projectDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", physTable))
	if err != nil {
		return ""
	}
	defer rows.Close()
	var found string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		if rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk) != nil {
			continue
		}
		if name == colName {
			found = colType
		}
	}
	// A failed iteration is handled like a failed query: no declared type.
	if rows.Err() != nil {
		return ""
	}
	return found
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
	rows, err := projectDB.Query(fmt.Sprintf("PRAGMA table_info(%s)", physTable))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		if rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk) != nil {
			continue
		}
		if strings.EqualFold(colType, "TEXT") && name != "id" && name != "created_at" {
			if !columnReadable(ep, name) {
				continue
			}
			cols = append(cols, name)
		}
	}
	// A failed iteration is handled like a failed query: no text columns.
	if rows.Err() != nil {
		return nil
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

	// Map column name -> declared type, for TEXT filtering.
	colTypes := make(map[string]string)
	rows, err := projectDB.Query("SELECT name, type FROM pragma_table_info(?)", physTable)
	if err != nil {
		return ""
	}
	for rows.Next() {
		var name, ctype string
		if rows.Scan(&name, &ctype) == nil {
			colTypes[name] = strings.ToUpper(strings.TrimSpace(ctype))
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return ""
	}

	// Walk the table's indexes and keep only single-column UNIQUE indexes
	// whose one column is TEXT and isn't "id".
	idxRows, err := projectDB.Query("SELECT name, \"unique\" FROM pragma_index_list(?)", physTable)
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
		colRows, err := projectDB.Query("SELECT name FROM pragma_index_info(?)", i.name)
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
		if colRows.Err() != nil {
			continue
		}
		if len(cols) != 1 {
			continue
		}
		c := cols[0]
		if c == "id" {
			continue
		}
		// SQLite's "TEXT" stays TEXT; also accept the empty type (declared
		// without an explicit type, treated as BLOB/TEXT affinity).
		t := colTypes[c]
		if t != "TEXT" && t != "" {
			continue
		}
		candidates = append(candidates, c)
	}

	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

// columnReadable reports whether a client may filter, sort or aggregate on col.
// Secure (PASSWORD/ENCRYPTED) columns never; otherwise public_columns bounds it
// when configured, so a hidden column cannot be probed through a query parameter.
func columnReadable(ep *apiEndpoint, col string) bool {
	if _, ok := ep.SecureCols[col]; ok {
		return false
	}
	if len(ep.PublicColumns) == 0 || col == "id" || col == "created_at" {
		return true
	}
	for _, c := range ep.PublicColumns {
		if c == col {
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
		allowed[c] = true
	}
	for col := range body {
		if !allowed[col] {
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
			if _, ok := ep.SecureCols[col]; ok {
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
// Security: validates each name, strips hash columns, intersects with
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
			allowed[col] = true
		}
	}

	hasID := false
	seen := make(map[string]bool)
	var cols []string
	for _, f := range strings.Split(raw, ",") {
		col := strings.TrimSpace(f)
		if col == "" || seen[col] {
			continue
		}
		if security.ValidateColumnName(col) != nil {
			continue
		}
		if _, ok := ep.SecureCols[col]; ok {
			continue
		}
		if allowed != nil && !allowed[col] {
			continue
		}
		seen[col] = true
		cols = append(cols, col)
		if col == "id" {
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

// scanRowsToMaps converts sql.Rows into a slice of maps.
func (h *Handler) scanRowsToMaps(rows *sql.Rows) []map[string]any {
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
		for col := range secureCols {
			delete(row, col)
		}
	}
}
