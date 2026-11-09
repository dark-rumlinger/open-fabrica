package public

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"openfabrica/security"
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
	if err != nil {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}

	// Authenticate and build the row-level security predicates the same way the
	// CRUD paths do. Without the owner/tenant filter an aggregation on a scoped
	// table would sum, count, or min/max across every owner's or tenant's rows.
	rc, _, ok := h.buildAPIRequestContext(w, r, getProject(r), ep)
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

	physTable := ep.TableName
	quotedGroupCols := make([]string, len(groupCols))
	for i, gc := range groupCols {
		quotedGroupCols[i] = security.QuoteIdent(gc)
	}
	var selectParts []string
	selectParts = append(selectParts, quotedGroupCols...)
	selectParts = append(selectParts, aggExpr+" AS result")
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(selectParts, ", "), security.QuoteIdent(physTable))

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

	if len(whereClauses) > 0 {
		query += " WHERE " + strings.Join(whereClauses, " AND ")
	}

	if len(groupCols) > 0 {
		query += " GROUP BY " + strings.Join(quotedGroupCols, ", ")
		query += " ORDER BY " + strings.Join(quotedGroupCols, ", ")
	}

	ctx, cancel := context.WithTimeout(r.Context(), dynamicAPIQueryTimeout)
	defer cancel()
	if len(groupCols) == 0 {
		var result float64
		err := projectDB.QueryRowContext(ctx, query, whereArgs...).Scan(&result) // #nosec G701 -- group/aggregate identifiers are QuoteIdent'd/validated; every value is a ? placeholder
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

	rows, err := projectDB.QueryContext(ctx, query, whereArgs...) // #nosec G701 -- group/aggregate identifiers are QuoteIdent'd/validated; every value is a ? placeholder
	if err != nil {
		writeError(w, http.StatusInternalServerError, "aggregation query failed")
		return
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		scanTargets := make([]any, len(groupCols)+1)
		values := make([]any, len(groupCols)+1)
		for i := range scanTargets {
			scanTargets[i] = &values[i]
		}
		if err := rows.Scan(scanTargets...); err != nil {
			continue
		}
		row := make(map[string]any)
		for i, gc := range groupCols {
			if b, ok := values[i].([]byte); ok {
				row[gc] = string(b)
			} else {
				row[gc] = values[i]
			}
		}
		row["result"] = values[len(groupCols)]
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "aggregation query failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"function": fn,
		"rows":     results,
	})
}
