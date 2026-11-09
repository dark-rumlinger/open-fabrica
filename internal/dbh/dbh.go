// Package dbh defines the database handle interface shared by the code that
// issues statements and the code that decides which pool serves them.
// Deliberately a leaf: it imports nothing but database/sql, so no cycles.
package dbh

import "database/sql"

// Handle is a database as its callers use it: statements and pool stats.
// *sql.DB satisfies it. Taking a Handle lets the per-project database hand out
// read- or write-routed pools; an implementation must not blur the method split:
// Query and QueryRow read, Exec and Begin write. A write smuggled through Query
// reaches the read pool, where concurrency violates SQLite's single-writer rule.
type Handle interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
	Begin() (*sql.Tx, error)
	Stats() sql.DBStats
}
