package data

import (
	"fmt"
	"regexp"
	"strings"

	"openfabrica/internal/sqlguard"
	"openfabrica/tools"
)

// systemTableRef matches a reference to a reserved of_ table: the prefix at an
// identifier boundary (start of token, after whitespace/comma/paren/dot), so an
// ordinary column like number_of_items - where of_ follows a word character -
// is not falsely rejected. Reserving the whole namespace this way means a new
// of_ table is blocked from raw SELECTs without touching this guard.
var systemTableRef = regexp.MustCompile(`(?i)\bof_\w*`)

// validateReadOnlySELECT enforces the shared "SELECT only, no system tables"
// contract for raw SQL the LLM supplies: execute_sql runs it directly, and
// create_view embeds it after CREATE VIEW ... AS. Returns the trimmed query
// (with any single trailing semicolon stripped) and a non-nil error message
// when the statement is rejected. rejecting multiple statements matters for
// create_view, where Exec could otherwise run an injected second statement.
func validateReadOnlySELECT(sqlQuery string) (string, string) {
	// Reject stacked statements first (a ';' that begins a second statement, ignoring
	// one inside a string/comment or a single trailing one) - it matters for
	// create_view, where Exec could otherwise run an injected write.
	if sqlguard.HasStackedStatements(sqlQuery) {
		return strings.TrimSpace(sqlQuery), "multiple SQL statements are not allowed (remove the ';')"
	}
	trimmed := strings.TrimSpace(sqlQuery)
	trimmed = strings.TrimRight(strings.TrimSpace(strings.TrimSuffix(trimmed, ";")), " \t\r\n")

	// Read-only only. EffectiveVerb resolves a leading `WITH` to the verb of the
	// statement AFTER its CTE definitions, so a read-only CTE (a recursive
	// `WITH ... SELECT` sequence generator, say) is allowed while a WITH-wrapped write
	// (`WITH ... INSERT/UPDATE/DELETE`) is still rejected - a literal "starts with
	// SELECT" check rejected the former and could be fooled by the latter.
	switch sqlguard.EffectiveVerb(trimmed) {
	case "SELECT", "VALUES":
		// read-only; fall through to the system-table check
	default:
		return trimmed, "only read-only queries are supported: SELECT, VALUES, or a WITH ... SELECT"
	}

	// Block access to system tables to prevent leaking secrets or internal
	// state. Any of_ table is reserved (see store.IsSystemTable); the regex
	// matches the prefix only at an identifier boundary so a user column such
	// as number_of_items is not mistaken for a system-table reference.
	if m := systemTableRef.FindString(trimmed); m != "" {
		return trimmed, fmt.Sprintf("cannot reference system table: %s", strings.ToLower(m))
	}
	return trimmed, ""
}

func DataExecuteSQL(ctx *tools.ToolContext, a *SQLExecute) (*tools.Result, error) {
	if a.SQL == "" {
		return tools.Fail("sql is required"), nil
	}

	trimmed, rejectErr := validateReadOnlySELECT(a.SQL)
	if rejectErr != "" {
		return tools.Fail("%s", "execute_sql: "+rejectErr), nil
	}

	// Default 100, hard-capped at 500 (the schema's default/max tags already
	// clamp tool-loop calls; this keeps direct callers on the same rule).
	limit := a.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	// Wrap rather than append, so the cap also holds when the query carries its own
	// (larger) LIMIT or merely mentions the word. The newlines keep a trailing
	// `-- comment` from swallowing the closing paren.
	finalSQL := fmt.Sprintf("SELECT * FROM (\n%s\n) LIMIT %d", trimmed, limit)

	rows, err := ctx.DB.Query(finalSQL)
	if err != nil {
		return tools.Fail("SQL error: %v", err), nil
	}
	defer rows.Close()

	results := ScanRowsMaps(rows)
	return tools.Ok(map[string]any{
		"count":   len(results),
		"results": results,
	}), nil
}
