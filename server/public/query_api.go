package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// validateReadOnlySelect enforces "single SELECT only, no system tables" on the
// SQL a client posts to the query endpoint. Returns the trimmed query (trailing
// ';' stripped) and a non-empty error message when rejected.
func validateReadOnlySelect(sqlQuery string) (string, string) {
	return sqlguard.CheckReadOnlySelect(sqlQuery)
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
	authTables, err := store.LoadAuthEndpointTables(projectDB)
	if err != nil {
		return unavailable, nil
	}
	for _, t := range authTables {
		blocked[strings.ToLower(t)] = true
	}
	// A failed lookup must not let the query through unguarded.
	hashTables, err := store.LoadHashTableNames(projectDB)
	if err != nil {
		return unavailable, nil
	}
	hashed, err := store.LoadHashColumnNames(projectDB)
	if err != nil {
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

// queryMemoryHogs are SQLite functions that let one anonymous request allocate
// hundreds of megabytes in a single value (randomblob(500000000)) or load code.
// The row cap and the timeout do not bound a single oversized value, and the public
// endpoint can be unauthenticated, so they are refused outright.
var queryMemoryHogs = map[string]bool{"randomblob": true, "zeroblob": true, "load_extension": true}

func (h *Handler) loadQueryEndpoint(projectDB *sql.DB, path string) (*store.QueryEndpoint, error) {
	return store.LoadQueryEndpoint(projectDB, path)
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
		if _, ok := h.projectUserClaims(project.ID, token); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	// Rate limit by IP. ClientIP only trusts X-Forwarded-For from a loopback
	// peer, so a remote client cannot spoof the header to dodge the per-IP limit.
	rateLimitKey := fmt.Sprintf("query:%d:%s:%s", project.ID, ep.Path, middleware.RateLimitIdentity(r))
	if !h.checkEndpointRateLimit(rateLimitKey, ep.RateLimit) {
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
	cols, out, truncated, err := store.ExecReadOnlyQuery(ctx, projectDB, finalSQL, hashed, limit)
	if err != nil {
		if errors.Is(err, store.ErrQueryScan) {
			writeError(w, http.StatusInternalServerError, "query error")
			return
		}
		// Surface the database error text, as a SQL tool should.
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
