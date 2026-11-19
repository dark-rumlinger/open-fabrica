package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// QueryEndpoint holds the opt-in configuration for a read-only SQL query endpoint.
type QueryEndpoint struct {
	Path         string
	MaxRows      int
	RequiresAuth bool
	RateLimit    int
}

// ErrQueryScan is returned by ExecReadOnlyQuery when scanning result rows fails
// (as opposed to a SQL execution error from the user's query).
var ErrQueryScan = errors.New("query result scan error")

// LoadAuthEndpointTables returns all table_name values from of_auth_endpoints.
func LoadAuthEndpointTables(reader *sql.DB) ([]string, error) {
	rows, err := reader.Query("SELECT table_name FROM of_auth_endpoints")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tables = append(tables, t)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

// LoadQueryEndpoint loads a single row from of_query_endpoints by path.
func LoadQueryEndpoint(reader *sql.DB, path string) (*QueryEndpoint, error) {
	var ep QueryEndpoint
	err := reader.QueryRow(
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

// ExecReadOnlyQuery runs a validated read-only SELECT query, drops any hashed
// columns from the result, and caps output at limit rows.
func ExecReadOnlyQuery(ctx context.Context, reader *sql.DB, finalSQL string, hashed map[string]bool, limit int) (cols []string, out [][]any, truncated bool, err error) {
	rows, err := reader.QueryContext(ctx, finalSQL)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()

	allCols, err := rows.Columns()
	if err != nil {
		return nil, nil, false, ErrQueryScan
	}

	var keep []int
	cols = make([]string, 0, len(allCols))
	for i, c := range allCols {
		if hashed[strings.ToLower(c)] {
			continue
		}
		keep = append(keep, i)
		cols = append(cols, c)
	}

	out = make([][]any, 0, min(limit, 1000))
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
			return nil, nil, false, ErrQueryScan
		}
		row := make([]any, len(cols))
		for i, idx := range keep {
			v := holders[idx]
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			} else {
				row[i] = v
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, ErrQueryScan
	}
	return cols, out, truncated, nil
}
