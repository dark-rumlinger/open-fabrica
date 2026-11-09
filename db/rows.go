package db

import (
	"database/sql"
	"log/slog"
)

// CollectRows runs scan for each row and returns the collected slice, closing rows
// when done. A row whose scan fails is skipped (logged at debug) so one malformed
// row can't blank a whole list response. Returns a nil slice when there are no
// rows; callers needing a JSON `[]` coalesce nil to empty at the edge.
func CollectRows[T any](rows *sql.Rows, scan func(*sql.Rows) (T, error)) []T {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			slog.Debug("CollectRows: skipping row that failed to scan", "error", err)
			continue
		}
		out = append(out, v)
	}
	// A mid-iteration error (e.g. a dropped connection) ends the loop without a
	// scan failure, silently truncating the list; surface it at warn since it
	// affects every row after the break, not just one malformed row.
	if err := rows.Err(); err != nil {
		slog.Warn("CollectRows: iteration failed; list truncated", "error", err)
	}
	return out
}
