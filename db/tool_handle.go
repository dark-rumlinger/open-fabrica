package db

import (
	"context"
	"database/sql"

	"openfabrica/internal/dbh"
	"openfabrica/internal/sqliteretry"
)

// toolHandle serves one caller's statements from a ProjectDB, reads from the
// read pool and writes from the write pool, every statement bound to one
// deadline.
type toolHandle struct {
	ctx    context.Context
	reader *sql.DB
	writer *sql.DB
}

// ToolHandle returns a Handle for a single tool call, bound to ctx.
//
// Reads go to the read pool, not the write connection: a caller given the write
// pool for both would deadlock when a read waits for the single connection its own
// open cursor is holding. Read-pool reads can't take the write connection, so they
// can't enter that cycle.
//
// Binding to ctx bounds the rest: a statement that blocks (saturated read pool, a
// lock) fails and releases its connection when ctx ends, since database/sql can
// only abandon a blocked statement given a context. Without it a blocked statement
// holds its connection for the life of the process.
func (s *ProjectDB) ToolHandle(ctx context.Context) dbh.Handle {
	return &toolHandle{ctx: ctx, reader: s.reader, writer: s.writer}
}

func (h *toolHandle) Query(query string, args ...any) (*sql.Rows, error) {
	return sqliteretry.QueryContext(h.ctx, h.reader, query, args...)
}

func (h *toolHandle) QueryRow(query string, args ...any) *sql.Row {
	return h.reader.QueryRowContext(h.ctx, query, args...)
}

func (h *toolHandle) Exec(query string, args ...any) (sql.Result, error) {
	return sqliteretry.ExecContext(h.ctx, h.writer, query, args...)
}

func (h *toolHandle) Begin() (*sql.Tx, error) {
	return sqliteretry.BeginTxContext(h.ctx, h.writer)
}

// Stats reports the write pool. It is the capped one, so it is the pool whose
// saturation says anything.
func (h *toolHandle) Stats() sql.DBStats { return h.writer.Stats() }
