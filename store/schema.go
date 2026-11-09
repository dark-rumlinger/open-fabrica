package store

// Cross-family schema utilities: the single source of truth for SQL identifier
// regexes, the reserved-name list, structured filter parsing, and the column
// schema/secure-column projection a row read needs.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/internal/dbh"
	"openfabrica/security"
)

// ValidTableName aliases security.IdentifierRegex so every interpolation
// site shares one canonical SQL identifier definition.
var ValidTableName = security.IdentifierRegex

// ValidColumnName aliases security.IdentifierRegex for the same reason.
var ValidColumnName = security.IdentifierRegex

// SecureColumnKinds maps logical PASSWORD/ENCRYPTED column types to the
// runtime handling they need (hash on write for PASSWORD, AES on
// write/decrypt on read for ENCRYPTED). Exported so the DDL package can
// reuse the same classification without redeclaring the map.
var SecureColumnKinds = map[string]string{
	"PASSWORD":  "hash",
	"ENCRYPTED": "encrypt",
}

// SystemTablePrefix is the reserved namespace for every internal table.
// Reserving the whole namespace (rather than enumerating tables) means a newly
// added of_* table is protected automatically, with no registry to keep in sync.
const SystemTablePrefix = "of_"

// reservedExactNames are internal tables outside the of_ namespace and so not
// caught by the prefix rule. Reserving the name stops an LLM-created table from
// colliding with the migration tracker.
var reservedExactNames = map[string]bool{
	"migrations": true,
}

// IsSystemTable returns true if the name is a reserved internal table. Any
// table in the of_ namespace is reserved, plus a short list of legacy
// non-prefixed names (see reservedExactNames): the LLM must not create,
// alter, drop, or SELECT from it, and the admin add-column path refuses it too.
func IsSystemTable(name string) bool {
	// SQLite resolves identifiers case-insensitively, so a write to "OF_ws_endpoints"
	// hits the same physical table as "of_ws_endpoints". Fold before matching so the
	// namespace guard can't be dodged by varying case.
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, SystemTablePrefix) || reservedExactNames[lower]
}

// SanitizedTableName validates and returns the physical table name, rejecting
// invalid identifiers and the reserved of_ system namespace.
func SanitizedTableName(name string) (string, error) {
	if !ValidTableName.MatchString(name) {
		return "", fmt.Errorf("invalid table name: %s (must be alphanumeric/underscore, start with letter)", name)
	}
	if IsSystemTable(name) {
		if strings.HasPrefix(name, SystemTablePrefix) {
			return "", fmt.Errorf("cannot use reserved system table name: %s (the of_ prefix is reserved for platform tables)", name)
		}
		return "", fmt.Errorf("cannot use reserved system table name: %s", name)
	}
	if err := security.ValidateIdentifierNotReserved("table", name); err != nil {
		return "", err
	}
	return name, nil
}

// AllowedSQLOps are the SQL comparison operators permitted in structured
// filters. Public so external callers (feeds, calendars) can validate
// operator strings against the same allow-list before passing them to
// BuildWhereClause.
var AllowedSQLOps = map[string]bool{
	"=": true, "!=": true, "<": true, ">": true, "<=": true, ">=": true,
	"LIKE": true, "NOT LIKE": true, "IS NULL": true, "IS NOT NULL": true,
}

// BuildWhereClause builds a parameterized WHERE clause from structured filters,
// prefixed with " WHERE " when any filter is provided (else ""). Column
// identifiers are validated; values are bound as parameters so the returned
// clause is safe to interpolate directly into a query.
func BuildWhereClause(filters []any) (string, []any, error) {
	if len(filters) == 0 {
		return "", nil, nil
	}

	var clauses []string
	var params []any

	for _, f := range filters {
		fm, ok := f.(map[string]any)
		if !ok {
			return "", nil, fmt.Errorf("each filter must be an object with column, op, and value")
		}
		col, _ := fm["column"].(string)
		op, _ := fm["op"].(string)

		if !ValidColumnName.MatchString(col) {
			return "", nil, fmt.Errorf("invalid column name in filter: %s", col)
		}
		op = strings.ToUpper(strings.TrimSpace(op))
		if !AllowedSQLOps[op] {
			return "", nil, fmt.Errorf("unsupported operator: %s. Allowed: =, !=, <, >, <=, >=, LIKE, NOT LIKE, IS NULL, IS NOT NULL; filters combine with AND only, so for IN, OR or BETWEEN use manage_sql", op)
		}

		if op == "IS NULL" || op == "IS NOT NULL" {
			clauses = append(clauses, fmt.Sprintf("%s %s", col, op))
		} else {
			val, hasVal := fm["value"]
			if !hasVal {
				return "", nil, fmt.Errorf("filter on %s requires a value", col)
			}
			clauses = append(clauses, fmt.Sprintf("%s %s ?", col, op))
			params = append(params, val)
		}
	}
	return " WHERE " + strings.Join(clauses, " AND "), params, nil
}

// GetTableColumns returns the set of physical column names on a project
// table, derived from PRAGMA table_info. Empty map (not nil) when the table
// has no rows in sqlite_master; callers branch on len.
func GetTableColumns(db dbh.Handle, physicalName string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", physicalName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// ColumnInfo describes one physical column exactly as SQLite reports it via
// PRAGMA table_info - the live schema read back from the database rather than
// the column-defs the caller was handed at create time.
type ColumnInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null,omitempty"`
	Default string `json:"default,omitempty"`
}

// GetTableColumnInfo returns the physical columns of a table in definition
// order with type/NOT NULL/default - the live schema read back from SQLite.
// Distinct from GetTableColumns (a bare name set): callers that need to show
// the real response shape (the CRUD row, the verified created table) use this.
func GetTableColumnInfo(db dbh.Handle, physicalName string) ([]ColumnInfo, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", physicalName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnInfo
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		ci := ColumnInfo{Name: name, Type: ctype, NotNull: notnull == 1}
		if dflt.Valid {
			ci.Default = dflt.String
		}
		out = append(out, ci)
	}
	return out, rows.Err()
}

// TableColumnNames returns the physical column names of a table in definition
// order, or nil on any introspection error (best-effort). Order matters:
// OwnerLikeColumn picks the first owner-like column, so a stable order gives a
// stable pick.
func TableColumnNames(db dbh.Handle, physicalName string) []string {
	cols, err := GetTableColumnInfo(db, physicalName)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		names = append(names, c.Name)
	}
	return names
}

// RequiredColumns returns the set of columns a caller MUST supply on insert:
// NOT NULL columns with no DEFAULT, excluding the rowid-alias primary key. A
// pre-flight check against this set turns a mid-batch DB error into an upfront,
// all-rows report. Empty map (not nil) when the table has none.
func RequiredColumns(db dbh.Handle, physicalName string) (map[string]bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", physicalName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	required := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		// pk==1 with no default is the rowid alias SQLite auto-fills; never required.
		if notnull == 1 && !dflt.Valid && pk == 0 {
			required[name] = true
		}
	}
	return required, rows.Err()
}

// LoadSecureColumns reads the PASSWORD/ENCRYPTED column projection for one table
// from of_dynamic_tables.secure_columns so callers can branch on a per-column
// kind ("hash"/"encrypt") at insert and select time; filtered defensively to
// known kinds. Empty map when the table has none; a real query error is surfaced.
func LoadSecureColumns(db dbh.Handle, tableName string) (map[string]string, error) {
	var raw sql.NullString
	err := db.QueryRow(
		"SELECT secure_columns FROM of_dynamic_tables WHERE table_name = ?",
		tableName,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return map[string]string{}, nil
	}
	return parseSecureColumns(raw.String), nil
}

// parseSecureColumns turns one stored secure_columns blob into a {column: kind}
// projection, filtering defensively to known kinds. Malformed JSON reports no
// secure columns; a caller that must not guess relies on the surrounding query's error.
func parseSecureColumns(raw string) map[string]string {
	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return map[string]string{}
	}
	knownKinds := map[string]bool{}
	for _, kind := range SecureColumnKinds {
		knownKinds[kind] = true
	}
	out := map[string]string{}
	for col, kind := range parsed {
		k := strings.ToLower(kind)
		if knownKinds[k] {
			out[col] = k
		}
	}
	return out
}

// LoadHashColumnNames returns the project-wide set of PASSWORD column names,
// lowercased, across every registered table. It is a union, so a name registered
// as PASSWORD on any table is treated as secret everywhere: the set errs toward
// hiding a safe column, never toward disclosing a hash. A query failure is
// surfaced rather than reported as an empty set, so the caller can fail closed.
func LoadHashColumnNames(db dbh.Handle) (map[string]bool, error) {
	rows, err := db.Query("SELECT secure_columns FROM of_dynamic_tables")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if !raw.Valid || raw.String == "" {
			continue
		}
		for col, kind := range parseSecureColumns(raw.String) {
			if kind == SecureColumnKinds["PASSWORD"] {
				out[strings.ToLower(col)] = true
			}
		}
	}
	return out, rows.Err()
}

// LoadHashTableNames returns the lowercased names of every dynamic table that
// holds a PASSWORD column. A query surface that must never expose a hash blocks
// these tables by name: filtering result columns by name alone is defeated by
// renaming (a CTE column list, an alias), while a table that is never read cannot
// leak. A query failure is surfaced so the caller can fail closed.
func LoadHashTableNames(db dbh.Handle) (map[string]bool, error) {
	rows, err := db.Query("SELECT table_name, secure_columns FROM of_dynamic_tables")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		var raw sql.NullString
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		if !raw.Valid || raw.String == "" {
			continue
		}
		for _, kind := range parseSecureColumns(raw.String) {
			if kind == SecureColumnKinds["PASSWORD"] {
				out[strings.ToLower(name)] = true
				break
			}
		}
	}
	return out, rows.Err()
}

// LoadTableSchema returns the set of column names on a dynamic table, sourced
// from of_dynamic_tables.schema_def rather than PRAGMA so logical column types
// (PASSWORD/ENCRYPTED) are respected. Empty set on missing row; other errors surfaced.
func LoadTableSchema(db *sql.DB, tableName string) (map[string]struct{}, error) {
	var raw sql.NullString
	err := db.QueryRow(
		"SELECT schema_def FROM of_dynamic_tables WHERE table_name = ?",
		tableName,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return map[string]struct{}{}, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw.String), &parsed); err != nil {
		return map[string]struct{}{}, nil
	}
	out := make(map[string]struct{}, len(parsed))
	for col := range parsed {
		out[col] = struct{}{}
	}
	return out, nil
}

// ProcessSecureValue applies the runtime handling implied by a column's
// SecureColumnKinds entry: bcrypt for "hash", AES for "encrypt". Returns the raw
// value unchanged for empty kind so callers can pass every column through without branching.
func ProcessSecureValue(kind string, value any, enc *security.Encryptor) (any, error) {
	switch kind {
	case "hash":
		s, _ := value.(string)
		return security.HashPassword(s)
	case "encrypt":
		if enc == nil {
			return nil, fmt.Errorf("encryption requested but encryptor not configured")
		}
		s, _ := value.(string)
		return enc.Encrypt(s)
	}
	return value, nil
}
