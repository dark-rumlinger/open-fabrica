// Package db is the low-level SQLite access layer. It opens the pooled global and
// per-project connections, manages the per-project database lifecycle, applies
// forward-only embedded migrations at open time, and defines the row models.
// Higher-level domain persistence lives in package store; this package stays
// close to database/sql.
package db
