// Package projectwrite is the single validated INSERT/UPDATE builder shared by
// the automation trigger runner and the authoritative room actor, so the
// SQL-injection surface and the $increment/$decrement semantics are defined in
// exactly one place: identifiers validated, every value bound as a parameter.
// Table/column POLICY (system-table and secure-column rejection) is the CALLER's job.
package projectwrite

import (
	"database/sql"
	"fmt"
	"strings"

	"openfabrica/security"
)

// validIdentifier is the canonical SQL identifier definition (shared with the
// data/DDL layer via security.IdentifierRegex). A name that matches is safe to
// interpolate; every value is still bound as a ? parameter.
var validIdentifier = security.IdentifierRegex

// Execer is the minimal write surface satisfied by both *sql.DB and *sql.Tx, so
// a caller can run a single write directly or batch several inside one tx.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Insert validates the table and every column name, then inserts one row with
// all values bound as parameters.
func Insert(exec Execer, table string, data map[string]any) (sql.Result, error) {
	if table == "" || len(data) == 0 {
		return nil, fmt.Errorf("insert: 'table' and a non-empty row are required")
	}
	if !validIdentifier.MatchString(table) {
		return nil, fmt.Errorf("insert: invalid table name: %s", table)
	}

	cols := make([]string, 0, len(data))
	placeholders := make([]string, 0, len(data))
	vals := make([]any, 0, len(data))
	for col, val := range data {
		if !validIdentifier.MatchString(col) {
			return nil, fmt.Errorf("insert: invalid column name: %s", col)
		}
		cols = append(cols, col)
		placeholders = append(placeholders, "?")
		vals = append(vals, val)
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	return exec.Exec(query, vals...)
}

// Update validates identifiers and updates rows matching where. A set value of
// {"$increment": n} generates "col = col + ?" and {"$decrement": n} generates
// "col = col - ?" (atomic counters that avoid a read-modify-write race); any
// other value is a plain "col = ?" assignment. All values bind as parameters.
func Update(exec Execer, table string, set, where map[string]any) (sql.Result, error) {
	if table == "" || len(set) == 0 || len(where) == 0 {
		return nil, fmt.Errorf("update: 'table', a non-empty set, and a non-empty where are required (got table=%q, set keys=%d, where keys=%d)", table, len(set), len(where))
	}
	if !validIdentifier.MatchString(table) {
		return nil, fmt.Errorf("update: invalid table name: %s", table)
	}

	setClauses := make([]string, 0, len(set))
	vals := make([]any, 0, len(set)+len(where))
	for col, val := range set {
		if !validIdentifier.MatchString(col) {
			return nil, fmt.Errorf("update: invalid column name in set: %s", col)
		}
		if obj, ok := val.(map[string]any); ok {
			if inc, ok := obj["$increment"]; ok {
				setClauses = append(setClauses, col+" = "+col+" + ?")
				vals = append(vals, inc)
				continue
			}
			if dec, ok := obj["$decrement"]; ok {
				setClauses = append(setClauses, col+" = "+col+" - ?")
				vals = append(vals, dec)
				continue
			}
		}
		setClauses = append(setClauses, col+" = ?")
		vals = append(vals, val)
	}

	whereClauses := make([]string, 0, len(where))
	for col, val := range where {
		if !validIdentifier.MatchString(col) {
			return nil, fmt.Errorf("update: invalid column name in where: %s", col)
		}
		whereClauses = append(whereClauses, col+" = ?")
		vals = append(vals, val)
	}

	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		table, strings.Join(setClauses, ", "), strings.Join(whereClauses, " AND "))
	return exec.Exec(query, vals...)
}

// Upsert validates the table, every column, and the conflict target, then
// inserts one row; on a conflict over conflictCols it updates the other columns
// (DO NOTHING when there are none). Conflict columns must all be present in
// data, are never overwritten, and must back a UNIQUE or PK index. Every value
// binds as a parameter.
func Upsert(exec Execer, table string, data map[string]any, conflictCols []string) (sql.Result, error) {
	if table == "" || len(data) == 0 || len(conflictCols) == 0 {
		return nil, fmt.Errorf("upsert: 'table', a non-empty row, and non-empty conflict columns are required (got table=%q, row keys=%d, conflict cols=%d)", table, len(data), len(conflictCols))
	}
	if !validIdentifier.MatchString(table) {
		return nil, fmt.Errorf("upsert: invalid table name: %s", table)
	}
	conflict := make(map[string]bool, len(conflictCols))
	for _, c := range conflictCols {
		if !validIdentifier.MatchString(c) {
			return nil, fmt.Errorf("upsert: invalid conflict column: %s", c)
		}
		conflict[c] = true
	}

	cols := make([]string, 0, len(data))
	placeholders := make([]string, 0, len(data))
	vals := make([]any, 0, len(data))
	setClauses := make([]string, 0, len(data))
	for col, val := range data {
		if !validIdentifier.MatchString(col) {
			return nil, fmt.Errorf("upsert: invalid column name: %s", col)
		}
		// {"$increment": n} / {"$decrement": n}: a fresh row starts at +n / -n, and a
		// conflicting row adds it atomically ("wins + 1" for a leaderboard), the same
		// counter semantics Update offers. Binding the object itself as a value would
		// fail the write.
		delta, isCounter, err := counterDelta(col, val)
		if err != nil {
			return nil, fmt.Errorf("upsert: %w", err)
		}
		if isCounter && conflict[col] {
			return nil, fmt.Errorf("upsert: conflict column %q cannot be a counter", col)
		}
		cols = append(cols, col)
		placeholders = append(placeholders, "?")
		if isCounter {
			vals = append(vals, delta)
		} else {
			vals = append(vals, val)
		}
		if !conflict[col] {
			if isCounter {
				// Unqualified col is the existing row's value; excluded.col is the delta.
				setClauses = append(setClauses, col+" = "+col+" + excluded."+col)
			} else {
				// excluded.<col> is the value that would have been inserted.
				setClauses = append(setClauses, col+" = excluded."+col)
			}
		}
	}
	// Every conflict column must be part of the row - it names the key being written.
	for _, c := range conflictCols {
		if !containsStr(cols, c) {
			return nil, fmt.Errorf("upsert: conflict column %q is missing from the row", c)
		}
	}

	action := "DO NOTHING"
	if len(setClauses) > 0 {
		action = "DO UPDATE SET " + strings.Join(setClauses, ", ")
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) %s",
		table, strings.Join(cols, ", "), strings.Join(placeholders, ", "),
		strings.Join(conflictCols, ", "), action)
	return exec.Exec(query, vals...)
}

// counterDelta reports whether val is a {"$increment": n} / {"$decrement": n} counter
// and, if so, the signed delta to write (a decrement is stored as -n).
func counterDelta(col string, val any) (any, bool, error) {
	obj, ok := val.(map[string]any)
	if !ok {
		return nil, false, nil
	}
	if inc, ok := obj["$increment"]; ok {
		return inc, true, nil
	}
	dec, ok := obj["$decrement"]
	if !ok {
		return nil, false, nil
	}
	switch n := dec.(type) {
	case int:
		return -n, true, nil
	case int64:
		return -n, true, nil
	case float64:
		return -n, true, nil
	}
	return nil, false, fmt.Errorf("column %q: $decrement needs a number, got %T", col, dec)
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
