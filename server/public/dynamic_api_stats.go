package public

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"openfabrica/security"
	"openfabrica/store"
)

// allowedAggFuncsPublic is the whitelist of aggregate functions for the public API.
var allowedAggFuncsPublic = map[string]bool{
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
}

// apiStats handles GET /api/{path}/_stats: aggregation queries.
// Query params: fn (required), column (required for sum/avg/min/max), group_by (comma-separated).
// Also supports same column=value filters as apiList.
func (h *Handler) apiStats(w http.ResponseWriter, r *http.Request, projectDB *sql.DB, endpointPath string) {
	ep, err := h.loadEndpoint(projectDB, endpointPath)
	if errors.Is(err, errEndpointMisconfigured) {
		writeError(w, http.StatusInternalServerError, "endpoint misconfigured")
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}

	// Authenticate and build the row-level security predicates the same way the
	// CRUD paths do. Without the owner/tenant filter an aggregation on a scoped
	// table would sum, count, or min/max across every owner's or tenant's rows.
	rc, ok := h.buildAPIRequestContext(w, r, getProject(r), ep)
	if !ok {
		return
	}

	q := r.URL.Query()
	fn := strings.ToLower(strings.TrimSpace(q.Get("fn")))
	if !allowedAggFuncsPublic[fn] {
		writeError(w, http.StatusBadRequest, "fn is required: count, sum, avg, min, or max")
		return
	}

	col := q.Get("column")
	if fn != "count" && col == "" {
		writeError(w, http.StatusBadRequest, fn+" requires a column parameter")
		return
	}
	if col != "" {
		if err := security.ValidateColumnName(col); err != nil {
			writeError(w, http.StatusBadRequest, "invalid column name")
			return
		}
		if kind, ok := ep.SecureCols[col]; ok && kind == "hash" {
			writeError(w, http.StatusBadRequest, "cannot aggregate on secure columns")
			return
		}
		if !columnReadable(ep, col) {
			writeError(w, http.StatusBadRequest, "column not available: "+col)
			return
		}
	}

	aggExpr := "COUNT(*)"
	if fn != "count" {
		aggExpr = fmt.Sprintf("%s(%s)", strings.ToUpper(fn), security.QuoteIdent(col))
	} else if col != "" {
		aggExpr = fmt.Sprintf("COUNT(%s)", security.QuoteIdent(col))
	}

	var groupCols []string
	if gb := q.Get("group_by"); gb != "" {
		for _, gc := range strings.Split(gb, ",") {
			gc = strings.TrimSpace(gc)
			if gc == "" {
				continue
			}
			if err := security.ValidateColumnName(gc); err != nil {
				writeError(w, http.StatusBadRequest, "invalid group_by column: "+gc)
				return
			}
			if kind, ok := ep.SecureCols[gc]; ok && kind == "hash" {
				writeError(w, http.StatusBadRequest, "cannot group by secure columns")
				return
			}
			if !columnReadable(ep, gc) {
				writeError(w, http.StatusBadRequest, "column not available: "+gc)
				return
			}
			groupCols = append(groupCols, gc)
		}
	}

	var whereClauses []string
	var whereArgs []any
	statsReserved := map[string]bool{"fn": true, "column": true, "group_by": true}
	for key, vals := range q {
		if reservedParams[key] || statsReserved[key] || len(vals) == 0 {
			continue
		}
		if err := security.ValidateColumnName(key); err != nil {
			continue
		}
		if kind, ok := ep.SecureCols[key]; ok && kind == "hash" {
			continue
		}
		if !columnReadable(ep, key) && columnExists(projectDB, ep.TableName, key) {
			writeError(w, http.StatusBadRequest, "column not available: "+key)
			return
		}
		whereClauses = append(whereClauses, security.QuoteIdent(key)+" = ?")
		whereArgs = append(whereArgs, vals[0])
	}

	// Row-level security: scope the aggregation to the caller's owner/tenant,
	// matching apiList. The leading " AND " is stripped because the clauses are
	// joined with AND below.
	if rc.OwnerFilter != "" {
		whereClauses = append(whereClauses, strings.TrimPrefix(rc.OwnerFilter, " AND "))
		whereArgs = append(whereArgs, rc.OwnerFilterVal)
	}
	if rc.TenantFilter != "" {
		whereClauses = append(whereClauses, strings.TrimPrefix(rc.TenantFilter, " AND "))
		whereArgs = append(whereArgs, rc.TenantFilterVal)
	}

	statsQuery := store.DynamicStatsQuery{
		Table:        ep.TableName,
		AggExpr:      aggExpr,
		GroupCols:    groupCols,
		WhereClauses: whereClauses,
		WhereArgs:    whereArgs,
	}

	ctx, cancel := context.WithTimeout(r.Context(), dynamicAPIQueryTimeout)
	defer cancel()
	if len(groupCols) == 0 {
		result, err := store.QueryDynamicStatsScalar(ctx, projectDB, statsQuery)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "aggregation query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"function": fn,
			"result":   result,
		})
		return
	}

	results, err := store.QueryDynamicStatsGrouped(ctx, projectDB, statsQuery)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "aggregation query failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"function": fn,
		"rows":     results,
	})
}
