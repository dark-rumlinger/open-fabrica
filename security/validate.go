package security

import (
	"fmt"
	"regexp"
	"strings"
)

// QuoteIdent wraps a SQL identifier in SQLite's double-quote form so it's
// safe to interpolate into a query string even if the caller forgot to run
// it through ValidateColumnName/ValidateTableName first. Embedded double
// quotes are doubled per the SQLite spec. This is defense in depth - the
// ValidateColumnName/ValidateTableName helpers are the primary line, this is
// the second.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// IdentifierRegex matches a SQL identifier safe for direct interpolation:
// must start with a letter, then alphanumeric/underscore. Exported so
// callers that prefer MatchString over an err-returning helper can use it
// directly. Table names and column names share this grammar in SQLite.
var IdentifierRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// sqliteReservedWords are SQLite keywords that cannot be used as unquoted
// identifiers without breaking at least one common code path (CREATE TABLE,
// ORDER BY <col>, etc). Several interpolation sites in this repo (the DDL
// builders, FTS virtual tables and their triggers) deliberately interpolate
// bare identifiers, so a reserved word would fail at execution time even
// though it passes the regex. We reject early so the LLM gets a clear,
// fixable error instead of a SQL syntax-error round-trip. (The public
// read/write API additionally wraps identifiers with QuoteIdent as defense in
// depth; rejecting reserved words keeps the unquoted paths consistent.)
// Source: https://sqlite.org/lang_keywords.html
var sqliteReservedWords = map[string]struct{}{
	"abort": {}, "action": {}, "add": {}, "after": {}, "all": {}, "alter": {},
	"always": {}, "analyze": {}, "and": {}, "as": {}, "asc": {}, "attach": {},
	"autoincrement": {}, "before": {}, "begin": {}, "between": {}, "by": {},
	"cascade": {}, "case": {}, "cast": {}, "check": {}, "collate": {},
	"column": {}, "commit": {}, "conflict": {}, "constraint": {}, "create": {},
	"cross": {}, "current": {}, "current_date": {}, "current_time": {},
	"current_timestamp": {}, "database": {}, "default": {}, "deferrable": {},
	"deferred": {}, "delete": {}, "desc": {}, "detach": {}, "distinct": {},
	"do": {}, "drop": {}, "each": {}, "else": {}, "end": {}, "escape": {},
	"except": {}, "exclude": {}, "exclusive": {}, "exists": {}, "explain": {},
	"fail": {}, "filter": {}, "first": {}, "following": {}, "for": {},
	"foreign": {}, "from": {}, "full": {}, "generated": {}, "glob": {},
	"group": {}, "groups": {}, "having": {}, "if": {}, "ignore": {},
	"immediate": {}, "in": {}, "index": {}, "indexed": {}, "initially": {},
	"inner": {}, "insert": {}, "instead": {}, "intersect": {}, "into": {},
	"is": {}, "isnull": {}, "join": {}, "key": {}, "last": {}, "left": {},
	"like": {}, "limit": {}, "match": {}, "materialized": {}, "natural": {},
	"no": {}, "not": {}, "nothing": {}, "notnull": {}, "null": {}, "nulls": {},
	"of": {}, "offset": {}, "on": {}, "or": {}, "order": {}, "others": {},
	"outer": {}, "over": {}, "partition": {}, "plan": {}, "pragma": {},
	"preceding": {}, "primary": {}, "query": {}, "raise": {}, "range": {},
	"recursive": {}, "references": {}, "regexp": {}, "reindex": {},
	"release": {}, "rename": {}, "replace": {}, "restrict": {}, "returning": {},
	"right": {}, "rollback": {}, "row": {}, "rows": {}, "savepoint": {},
	"select": {}, "set": {}, "table": {}, "temp": {}, "temporary": {},
	"then": {}, "ties": {}, "to": {}, "transaction": {}, "trigger": {},
	"unbounded": {}, "union": {}, "unique": {}, "update": {}, "using": {},
	"vacuum": {}, "values": {}, "view": {}, "virtual": {}, "when": {},
	"where": {}, "window": {}, "with": {}, "without": {},
}

// reservedWordSuggestions points the LLM at non-reserved alternatives for
// the handful of keywords it picks most often as column names. The map is
// only consulted to enrich the error message; absence here is fine, the
// error still says the name is reserved.
var reservedWordSuggestions = map[string]string{
	"order":      "sort_order, position, rank",
	"group":      "group_name, category, bucket",
	"key":        "key_name, lookup_key, slug",
	"index":      "position, idx, sort_order",
	"from":       "from_address, source, origin",
	"to":         "to_address, destination, target",
	"default":    "default_value, fallback, is_default",
	"references": "ref, reference_id, source_ref",
	"check":      "check_name, validation, is_checked",
	"range":      "range_start, span, bounds",
	"limit":      "max_count, cap, threshold",
	"offset":     "skip, start_at, page_offset",
	"union":      "union_type, combined, group_kind",
	"select":     "selection, chosen, picked",
	"table":      "table_name, sheet, dataset",
	"join":       "join_type, link, association",
	"where":      "location, where_clause, condition",
	"when":       "occurred_at, event_time, scheduled_at",
	"window":     "window_size, frame, interval",
	"row":        "row_index, line, entry",
	"with":       "associated_with, related_to, tags",
}

// IsSQLiteReservedWord reports whether name (case-insensitive) is a SQLite
// keyword that would break unquoted interpolation. Callers that want to
// emit their own error use this directly; ValidateIdentifierNotReserved
// returns a ready-made error including suggested alternatives.
func IsSQLiteReservedWord(name string) bool {
	_, ok := sqliteReservedWords[strings.ToLower(name)]
	return ok
}

// ValidateIdentifierNotReserved returns an LLM-friendly error if name is a
// SQLite reserved word. The error names the kind ("column"/"table") and,
// when known, suggests concrete renames so the model can self-correct in
// one tool call rather than waiting for the runtime SQL syntax error.
func ValidateIdentifierNotReserved(kind, name string) error {
	if !IsSQLiteReservedWord(name) {
		return nil
	}
	lower := strings.ToLower(name)
	if hint, ok := reservedWordSuggestions[lower]; ok {
		return fmt.Errorf("invalid %s name: %q is a SQLite reserved word - use one of: %s", kind, name, hint)
	}
	return fmt.Errorf("invalid %s name: %q is a SQLite reserved word - prefix or rename it (e.g. %s_name, %s_value)", kind, name, lower, lower)
}

// ValidateColumnName checks that a column name is safe for SQL interpolation.
// Returns an error if the name contains characters outside [a-zA-Z0-9_] or
// does not start with a letter.
func ValidateColumnName(name string) error {
	if !IdentifierRegex.MatchString(name) {
		return fmt.Errorf("invalid column name: %q (must start with letter, contain only alphanumeric/underscore)", name)
	}
	return ValidateIdentifierNotReserved("column", name)
}

// ValidateColumnNames validates a slice of column names for SQL safety.
func ValidateColumnNames(names []string) error {
	for _, name := range names {
		if err := ValidateColumnName(name); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTableName checks that a table name is safe for SQL interpolation.
// SQLite table identifiers share the same grammar as column identifiers, so
// this delegates to the same regex. Distinct function for clarity at call sites.
func ValidateTableName(name string) error {
	if !IdentifierRegex.MatchString(name) {
		return fmt.Errorf("invalid table name: %q (must start with letter, contain only alphanumeric/underscore)", name)
	}
	return ValidateIdentifierNotReserved("table", name)
}

// fkReferenceRegex matches a foreign key target in the form "table(column)".
var fkReferenceRegex = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*\([a-zA-Z][a-zA-Z0-9_]*\)$`)

// ValidateFKReference checks that a foreign key reference of the form
// "table(column)" uses safe identifiers on both sides.
func ValidateFKReference(ref string) error {
	if !fkReferenceRegex.MatchString(ref) {
		return fmt.Errorf("invalid foreign key reference: %q (expected form 'table(column)')", ref)
	}
	return nil
}

// OwnerFilterClause returns a validated SQL fragment of the form
// "<column> = ?" for the given owner column. The returned fragment has no
// leading separator; callers compose it with WHERE or AND as needed. Returns
// an empty string and nil error when column is empty (no filter applies).
// Centralising this prevents per-call-site interpolation of unvalidated
// identifiers into RLS-critical queries.
func OwnerFilterClause(column string) (string, error) {
	if column == "" {
		return "", nil
	}
	if err := ValidateColumnName(column); err != nil {
		return "", err
	}
	return QuoteIdent(column) + " = ?", nil
}

// TenantFilterClause is the tenancy sibling of OwnerFilterClause. Validates the
// configured tenant_column name and returns "<column> = ?". Same separator
// rules: empty input -> empty output -> no clause appended. Centralised so
// every tenant-scoped query passes through the same identifier validator.
func TenantFilterClause(column string) (string, error) {
	if column == "" {
		return "", nil
	}
	if err := ValidateColumnName(column); err != nil {
		return "", err
	}
	return QuoteIdent(column) + " = ?", nil
}
