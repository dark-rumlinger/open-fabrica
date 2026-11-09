package db

import (
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func (db *DB) migrate() error {
	return applyMigrations(db.DB, db.writer, migrationFS, "migrations")
}

// applyMigrations applies every pending .sql file under dir in fsys, tracked in a
// `migrations` ledger table, one transaction per file (a crash or failing
// statement leaves a migration fully applied or not at all; SQLite DDL is
// transactional). reader counts applied migrations, writer runs the DDL, and
// logCtx flavors the "applied migration" log line (e.g. the project id). Shared
// by the global and per-project runners.
func applyMigrations(reader, writer *sql.DB, fsys embed.FS, dir string, logCtx ...any) error {
	if _, err := writer.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading migrations dir %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		name := entry.Name()

		var count int
		if err := reader.QueryRow("SELECT COUNT(*) FROM migrations WHERE name = ?", name).Scan(&count); err != nil {
			return fmt.Errorf("checking migration %s: %w", name, err)
		}
		if count > 0 {
			continue
		}

		content, err := fsys.ReadFile(dir + "/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}

		tx, err := writer.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		// splitSQLStatements strips comments and respects string literals before
		// splitting on `;`, so prose like "-- step one; step two" stays one comment
		// instead of being chopped into a bare-keyword "statement" (see db/sql_split.go).
		for _, stmt := range splitSQLStatements(string(content)) {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("executing migration %s: %w\nStatement: %s", name, err, stmt)
			}
		}
		if _, err := tx.Exec("INSERT INTO migrations (name) VALUES (?)", name); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %s: %w", name, err)
		}

		slog.Info("applied migration", append([]any{"name", name}, logCtx...)...)
	}
	return nil
}
