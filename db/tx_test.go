package db

import (
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

// newScratchDB opens an in-memory SQLite DB with a single shared connection
// (so an ":memory:" database is not silently recreated per connection) and a
// one-column scratch table the transaction tests write to.
func newScratchDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func countItems(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n); err != nil {
		t.Fatalf("count items: %v", err)
	}
	return n
}

func TestRunInTx_CommitsOnSuccess(t *testing.T) {
	db := newScratchDB(t)
	err := RunInTx(db, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO items (id, name) VALUES (1, 'alice')`)
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	if n := countItems(t, db); n != 1 {
		t.Errorf("row count = %d, want 1 (commit did not persist)", n)
	}
}

func TestRunInTx_RollsBackOnError(t *testing.T) {
	db := newScratchDB(t)
	sentinel := errors.New("boom")
	err := RunInTx(db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO items (id, name) VALUES (1, 'alice')`); err != nil {
			return err
		}
		// Return an error after a successful write: RunInTx must roll back.
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunInTx returned %v, want sentinel error", err)
	}
	if n := countItems(t, db); n != 0 {
		t.Errorf("row count = %d, want 0 (rollback did not undo the write)", n)
	}
}

func TestRunInTx_RollsBackAndRepanicsOnPanic(t *testing.T) {
	db := newScratchDB(t)

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected RunInTx to re-panic, but it did not")
			}
			if s, ok := r.(string); !ok || s != "kaboom" {
				t.Fatalf("re-panicked with %v, want \"kaboom\"", r)
			}
		}()
		_ = RunInTx(db, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO items (id, name) VALUES (1, 'alice')`); err != nil {
				t.Fatalf("insert before panic: %v", err)
			}
			panic("kaboom")
		})
	}()

	// After the panic unwinds, the write must have been rolled back.
	if n := countItems(t, db); n != 0 {
		t.Errorf("row count = %d, want 0 (panic path did not roll back)", n)
	}
}
