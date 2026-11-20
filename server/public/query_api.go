package public

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// guardQueryTargets rejects SQL that reads tables the query endpoint never serves:
// engine internals (sqlite_*, pragma_*), payment records, the project's user
// tables, RLS-scoped tables the caller cannot bypass, and any secure (PASSWORD or
// ENCRYPTED) column. It returns the secure-column set so the caller can drop them
// from SELECT * output.
func guardQueryTargets(projectDB *sql.DB, query string, bypassOwner, bypassTenant bool) (string, map[string]bool) {
	const unavailable = "query guard unavailable, try again"
	blocked := map[string]bool{"of_payments": true, "of_payment_config": true}
	authTables, err := store.LoadAuthEndpointTables(projectDB)
	if err != nil {
		return unavailable, nil
	}
	for _, t := range authTables {
		blocked[strings.ToLower(t)] = true
	}
	if !bypassOwner || !bypassTenant {
		ownerScoped, tenantScoped, err := store.ListScopedAPITablesByKind(projectDB)
		if err != nil {
			return unavailable, nil
		}
		if !bypassOwner {
			for t := range ownerScoped {
				blocked[t] = true
			}
		}
		if !bypassTenant {
			for t := range tenantScoped {
				blocked[t] = true
			}
		}
	}
	// A failed lookup must not let the query through unguarded.
	secretTables, err := store.LoadSecureTableNames(projectDB)
	if err != nil {
		return unavailable, nil
	}
	secretCols, err := store.LoadSecureColumnNames(projectDB)
	if err != nil {
		return unavailable, nil
	}
	if msg := sqlguard.CheckQueryTargets(query, blocked, secretCols, secretTables); msg != "" {
		return msg, secretCols
	}
	return "", secretCols
}

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

	bypassOwner, bypassTenant := false, false
	if token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); token != "" && token != r.Header.Get("Authorization") {
		if claims, ok := h.projectUserClaims(project.ID, token); ok {
			bypassOwner = claims.Role == "admin"
			bypassTenant = claims.TenantRole == "platform_admin"
		} else if ep.RequiresAuth {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	} else if ep.RequiresAuth {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
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

	msg, secretCols := guardQueryTargets(projectDB, trimmed, bypassOwner, bypassTenant)
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
	cols, out, truncated, err := store.ExecReadOnlyQuery(ctx, projectDB, finalSQL, secretCols, limit)
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
