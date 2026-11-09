package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"openfabrica/db/models"
	"openfabrica/internal/sqlguard"
	"openfabrica/server/middleware"
	"openfabrica/store"
)

// Read-only raw-SELECT endpoint (opt-in). POST /api/<path> with
// {"sql":"SELECT ...","limit":N} runs a read-only SELECT against the project DB and
// returns {columns, rows, row_count, execution_ms} - the only runtime surface for
// joins/aggregations/views.
//
// Off unless a row exists in of_query_endpoints (via manage_api enable_query), since
// it grants read of every non-system table. The SELECT-only / no-system-table guard
// mirrors tools/data (validateReadOnlySELECT), replicated rather than imported so the
// public server keeps no build-time dependency.

const maxQuerySQLBytes = 16 * 1024 // 16 KB of SQL text is plenty for a SELECT.

// queryEndpointSystemTableRef matches a reference to a reserved of_ table at an
// identifier boundary, so an ordinary column like number_of_items is not
// falsely rejected. Mirrors data.systemTableRef.
var queryEndpointSystemTableRef = regexp.MustCompile(`(?i)\bof_\w*`)

// validateReadOnlySelect enforces "single SELECT only, no system tables" on the
// SQL a client posts to the query endpoint. Returns the trimmed query (trailing
// ';' stripped) and a non-empty error message when rejected.
func validateReadOnlySelect(sqlQuery string) (string, string) {
	trimmed := strings.TrimSpace(sqlQuery)
	trimmed = strings.TrimRight(strings.TrimSpace(strings.TrimSuffix(trimmed, ";")), " \t\r\n")

	if trimmed == "" {
		return trimmed, "sql is required"
	}
	// Verb + stacked-statement checks come from the shared sqlguard package (the
	// same guard the build-time manage_sql path uses), so this surface can't drift
	// into a weaker - or stricter - individually-maintained variant. sqlguard
	// correctly ignores a ';' inside a string literal or comment, so a legitimate
	// ';' in a value no longer false-rejects the query.
	//
	// EffectiveVerb, not LeadingVerb: a leading `WITH` resolves to the verb of the
	// statement AFTER its CTE definitions, so a read-only `WITH ... SELECT` is
	// accepted here exactly as manage_sql accepts it at build time, while a
	// WITH-wrapped write is still refused. A first-token check rejected a query the
	// model had already verified against the same data.
	switch sqlguard.EffectiveVerb(trimmed) {
	case "SELECT", "VALUES":
	default:
		return trimmed, "only read-only queries are supported: SELECT, VALUES, or a WITH ... SELECT"
	}
	if sqlguard.HasStackedStatements(trimmed) {
		return trimmed, "multiple SQL statements are not allowed (remove the ';')"
	}
	if m := queryEndpointSystemTableRef.FindString(trimmed); m != "" {
		return trimmed, fmt.Sprintf("cannot reference system table: %s", strings.ToLower(m))
	}
	return trimmed, ""
}

// queryIdentRef matches identifier-shaped tokens, used to spot a reference to a
// blocked table or column wherever it appears in the SQL (quoted or not).
var queryIdentRef = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// guardQueryTargets rejects SQL that reads tables the query endpoint never serves:
// engine internals (sqlite_*, pragma_*), payment records, the project's user
// tables, and any column that stores a password hash. A reference is matched by
// name anywhere in the text, so aliasing or quoting cannot hide it. It also
// returns the hash-column names so the caller can drop them from SELECT * output.
func guardQueryTargets(projectDB *sql.DB, query string) (string, map[string]bool) {
	const unavailable = "query guard unavailable, try again"
	blocked := map[string]bool{"of_payments": true, "of_payment_config": true}
	if rows, err := projectDB.Query("SELECT table_name FROM of_auth_endpoints"); err == nil {
		for rows.Next() {
			var t string
			if rows.Scan(&t) == nil {
				blocked[strings.ToLower(t)] = true
			}
		}
		iterErr := rows.Err()
		rows.Close()
		// A half-read list would leave user tables unblocked, so refuse the query.
		if iterErr != nil {
			return unavailable, nil
		}
	}
	// A lookup that fails for any reason but a missing registry (no dynamic table,
	// so no hash column, can exist yet) must not let the query through unguarded.
	hashTables, err := store.LoadHashTableNames(projectDB)
	if err != nil && !isNoSuchTable(err) {
		return unavailable, nil
	}
	hashed, err := store.LoadHashColumnNames(projectDB)
	if err != nil && !isNoSuchTable(err) {
		return unavailable, nil
	}
	toks := queryIdentRef.FindAllString(query, -1)
	touchesHashTable, compound := false, false
	for _, tok := range toks {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, "sqlite_") || strings.HasPrefix(low, "pragma_"):
			return fmt.Sprintf("cannot reference engine table: %s", low), hashed
		case blocked[low]:
			return fmt.Sprintf("cannot reference protected table: %s", low), hashed
		case hashed[low]:
			return fmt.Sprintf("cannot reference secret column: %s", low), hashed
		case queryMemoryHogs[low]:
			return fmt.Sprintf("function not allowed: %s", low), hashed
		}
		touchesHashTable = touchesHashTable || hashTables[low]
		compound = compound || queryCompoundWords[low]
	}
	// Hash columns are dropped from the result by NAME, which only holds while the
	// result keeps its column names. A compound query takes its names from another
	// branch (SELECT 1,2,3 UNION SELECT * FROM staff), and a CTE column list renames
	// outright, so against a table that holds a hash such queries are refused.
	if touchesHashTable && compound {
		return "queries over a table with a password column must be a plain SELECT (no WITH, UNION, INTERSECT, EXCEPT or VALUES)", hashed
	}
	return "", hashed
}

// queryCompoundWords are the keywords after which a result column's name no longer
// comes from the table it was read from.
var queryCompoundWords = map[string]bool{"with": true, "union": true, "intersect": true, "except": true, "values": true}

// isNoSuchTable reports a SQLite "no such table" error.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// queryMemoryHogs are SQLite functions that let one anonymous request allocate
// hundreds of megabytes in a single value (randomblob(500000000)) or load code.
// The row cap and the timeout do not bound a single oversized value, and the public
// endpoint can be unauthenticated, so they are refused outright.
var queryMemoryHogs = map[string]bool{"randomblob": true, "zeroblob": true, "load_extension": true}

type queryEndpoint struct {
	Path         string
	MaxRows      int
	RequiresAuth bool
	RateLimit    int
}

// loadQueryEndpoint returns the opt-in config for the query endpoint, or an
// error when the project has not enabled it.
func (h *Handler) loadQueryEndpoint(projectDB *sql.DB, path string) (*queryEndpoint, error) {
	var ep queryEndpoint
	err := projectDB.QueryRow(
		"SELECT path, COALESCE(max_rows,200), COALESCE(requires_auth,0), COALESCE(rate_limit,60) FROM of_query_endpoints WHERE path = ?",
		path,
	).Scan(&ep.Path, &ep.MaxRows, &ep.RequiresAuth, &ep.RateLimit)
	if err != nil {
		return nil, err
	}
	if ep.MaxRows <= 0 || ep.MaxRows > 1000 {
		ep.MaxRows = 200
	}
	return &ep, nil
}

type queryRequest struct {
	SQL   string `json:"sql"`
	Limit int    `json:"limit"`
}

// apiQuery handles POST /api/<path> for the read-only query endpoint.
func (h *Handler) apiQuery(w http.ResponseWriter, r *http.Request, project *models.Project, projectDB *sql.DB, path string) {
	ep, err := h.loadQueryEndpoint(projectDB, path)
	if err != nil {
		// Not opted in: behave like any unknown endpoint.
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}

	if ep.RequiresAuth {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !h.validateProjectTokenOrJWT(project.ID, token) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	// Rate limit by IP. ClientIP only trusts X-Forwarded-For from a loopback
	// peer, so a remote client cannot spoof the header to dodge the per-IP limit.
	rateLimitKey := fmt.Sprintf("query:%d:%s:%s", project.ID, ep.Path, middleware.RateLimitIdentity(r))
	if !checkEndpointRateLimit(rateLimitKey, ep.RateLimit) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}

	var req queryRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxQuerySQLBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	trimmed, rejectErr := validateReadOnlySelect(req.SQL)
	if rejectErr != "" {
		// 400 with the reason, mirroring the brief's "clear error message".
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": rejectErr})
		return
	}

	msg, hashed := guardQueryTargets(projectDB, trimmed)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}

	// The row cap is enforced while reading, not by editing the SQL: a text check
	// for LIMIT is fooled by a comment or string literal containing the word.
	limit := ep.MaxRows
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	finalSQL := trimmed

	ctx, cancel := context.WithTimeout(r.Context(), dynamicAPIQueryTimeout)
	defer cancel()

	start := time.Now()
	rows, err := projectDB.QueryContext(ctx, finalSQL)
	if err != nil {
		// Surface the database error text, as a SQL tool should.
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	defer rows.Close()

	allCols, err := rows.Columns()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query error")
		return
	}
	// A SELECT * can still surface a password-hash column no token named; drop any
	// result column that carries one.
	var keep []int
	cols := make([]string, 0, len(allCols))
	for i, c := range allCols {
		if hashed[strings.ToLower(c)] {
			continue
		}
		keep = append(keep, i)
		cols = append(cols, c)
	}

	out := make([][]any, 0, min(limit, 1000))
	truncated := false
	for rows.Next() {
		if len(out) >= limit {
			truncated = true
			break
		}
		holders := make([]any, len(allCols))
		ptrs := make([]any, len(allCols))
		for i := range holders {
			ptrs[i] = &holders[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			writeError(w, http.StatusInternalServerError, "query error")
			return
		}
		row := make([]any, len(cols))
		for i, idx := range keep {
			v := holders[idx]
			// The SQLite driver hands back []byte for TEXT/BLOB; render as a
			// string so the JSON grid shows readable values, not base64.
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			} else {
				row[i] = v
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"columns":      cols,
		"rows":         out,
		"row_count":    len(out),
		"truncated":    truncated,
		"execution_ms": float64(time.Since(start).Microseconds()) / 1000.0,
	})
}
