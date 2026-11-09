package db

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"openfabrica/internal/sqliteretry"
)

type DB struct {
	*sql.DB         // read pool (concurrent reads)
	writer  *sql.DB // write pool (single connection, serializes writes)
}

func Open(path string) (*DB, error) {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	readDB, writeDB, err := openReadWritePools(path)
	if err != nil {
		return nil, err
	}

	db := &DB{DB: readDB, writer: writeDB}

	// Run migrations
	if err := db.migrate(); err != nil {
		readDB.Close()
		writeDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	slog.Info("database initialized", "path", path)
	return db, nil
}

// Close closes both the read and write pools.
func (db *DB) Close() error {
	werr := db.writer.Close()
	if derr := db.DB.Close(); derr != nil {
		return derr
	}
	return werr
}

// ExecWrite executes a write query through the serialized writer pool with retry.
func (db *DB) ExecWrite(query string, args ...any) (sql.Result, error) {
	return sqliteretry.Exec(db.writer, query, args...)
}

// BeginWriteTx starts a transaction on the writer pool with retry.
func (db *DB) BeginWriteTx() (*sql.Tx, error) {
	return sqliteretry.BeginTx(db.writer)
}

// GetSetting retrieves a setting value by key
func (db *DB) GetSetting(key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	return value, err
}

// SetSetting sets a setting value
func (db *DB) SetSetting(key, value string) error {
	_, err := db.ExecWrite(
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value,
	)
	return err
}
