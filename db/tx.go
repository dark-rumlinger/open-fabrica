package db

import "database/sql"

// RunInTx runs fn inside a transaction on the given write pool, committing when fn
// returns nil and rolling back on any error or panic. Keeps a multi-step write path
// all-or-nothing without hand-rolling Begin/Rollback/Commit. fn runs its statements
// on the passed *sql.Tx.
func RunInTx(writer *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := writer.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// WithTx is RunInTx bound to this project's serialized writer pool.
func (s *ProjectDB) WithTx(fn func(*sql.Tx) error) error {
	return RunInTx(s.writer, fn)
}
