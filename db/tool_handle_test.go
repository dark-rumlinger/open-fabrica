package db

import (
	"context"
	"testing"
	"time"
)

// toolHandleDB returns a real project database with a `things` table seeded with
// n rows, plus a handle bound to ctx.
func toolHandleDB(t *testing.T, n int) *ProjectDB {
	t.Helper()
	mgr := NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(`CREATE TABLE things (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := pdb.ExecWrite(`INSERT INTO things (id, name) VALUES (?, ?)`, i, "row"); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	return pdb
}

// guard fails the test if fn does not return within d, so a routing regression
// surfaces as a clear failure rather than a hang.
func guard(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s", what, d)
	}
}

// The whole point of routing: a read taken while another read's cursor is open
// is served by the read pool, so it cannot wait on the write connection the way
// it would if both came from the single-connection write pool.
func TestToolHandle_NestedReadWhileCursorOpen(t *testing.T) {
	pdb := toolHandleDB(t, 3)
	h := pdb.ToolHandle(context.Background())

	guard(t, 10*time.Second, "nested read while cursor open", func() {
		rows, err := h.Query(`SELECT id FROM things ORDER BY id`)
		if err != nil {
			t.Errorf("outer query: %v", err)
			return
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				t.Errorf("scan: %v", err)
				return
			}
			// A nested read issued while the outer rows cursor is still open.
			var name string
			if err := h.QueryRow(`SELECT name FROM things WHERE id = ?`, id).Scan(&name); err != nil {
				t.Errorf("nested read for id %d: %v", id, err)
				return
			}
			seen++
		}
		if err := rows.Err(); err != nil {
			t.Errorf("iterate rows: %v", err)
		}
		if seen != 3 {
			t.Errorf("saw %d rows, want 3", seen)
		}
	})
}

// Reads and writes come from different pools, so a write must still be visible
// to the very next read. WAL gives each autocommit read a fresh snapshot, which
// is what makes the split safe to route.
func TestToolHandle_ReadSeesOwnWrite(t *testing.T) {
	pdb := toolHandleDB(t, 0)
	h := pdb.ToolHandle(context.Background())

	if _, err := h.Exec(`INSERT INTO things (id, name) VALUES (42, 'written')`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	var name string
	if err := h.QueryRow(`SELECT name FROM things WHERE id = 42`).Scan(&name); err != nil {
		t.Fatalf("read own write: %v", err)
	}
	if name != "written" {
		t.Errorf("name = %q, want %q", name, "written")
	}
}

// A write inside a handle transaction is visible through the handle once
// committed, so Begin still routes to the writer and commits normally.
func TestToolHandle_BeginCommitVisibleToReads(t *testing.T) {
	pdb := toolHandleDB(t, 0)
	h := pdb.ToolHandle(context.Background())

	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO things (id, name) VALUES (7, 'tx')`); err != nil {
		t.Fatalf("tx exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var name string
	if err := h.QueryRow(`SELECT name FROM things WHERE id = 7`).Scan(&name); err != nil {
		t.Fatalf("read after commit: %v", err)
	}
	if name != "tx" {
		t.Errorf("name = %q, want %q", name, "tx")
	}
}

// Reads are served from a different connection than an open write transaction,
// so they see the pre-transaction snapshot rather than its uncommitted rows.
//
// This pins the contract deliberately: a handler that must read its own
// uncommitted writes has to read through the tx. What matters here is that the
// read RETURNS - on the single-connection handle it could not, because the
// transaction held the only connection.
func TestToolHandle_ReadDuringOpenTxSeesPreTxSnapshot(t *testing.T) {
	pdb := toolHandleDB(t, 0)
	h := pdb.ToolHandle(context.Background())

	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO things (id, name) VALUES (99, 'uncommitted')`); err != nil {
		t.Fatalf("tx exec: %v", err)
	}

	guard(t, 10*time.Second, "read during open write tx", func() {
		var n int
		if err := h.QueryRow(`SELECT COUNT(*) FROM things WHERE id = 99`).Scan(&n); err != nil {
			t.Errorf("read during tx: %v", err)
			return
		}
		if n != 0 {
			t.Errorf("count = %d, want 0: an uncommitted row must not be visible outside its tx", n)
		}
		// The same read through the tx does see it.
		if err := tx.QueryRow(`SELECT COUNT(*) FROM things WHERE id = 99`).Scan(&n); err != nil {
			t.Errorf("read through tx: %v", err)
			return
		}
		if n != 1 {
			t.Errorf("count through tx = %d, want 1", n)
		}
	})
}

// Every statement is bound to the handle's context, so a caller whose deadline
// has passed gets an error instead of a connection held past it.
func TestToolHandle_ExpiredContextFailsFast(t *testing.T) {
	pdb := toolHandleDB(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before any statement runs
	h := pdb.ToolHandle(ctx)

	guard(t, 10*time.Second, "statements on an expired context", func() {
		rows, err := h.Query(`SELECT id FROM things`)
		if err == nil {
			t.Error("Query on a cancelled context returned no error")
		}
		if rows != nil {
			_ = rows.Err()
			rows.Close()
		}
		if err := h.QueryRow(`SELECT id FROM things`).Scan(new(int)); err == nil {
			t.Error("QueryRow on a cancelled context returned no error")
		}
		if _, err := h.Exec(`INSERT INTO things (id, name) VALUES (2, 'x')`); err == nil {
			t.Error("Exec on a cancelled context returned no error")
		}
		if _, err := h.Begin(); err == nil {
			t.Error("Begin on a cancelled context returned no error")
		}
	})
}
