package data

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupTransferDB stages two accounts that the transfer-shaped tests below
// debit/credit. Reused across success, rollback, and validation checks so
// every scenario starts from the same balance.
func setupTransferDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	stmts := []string{
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			holder TEXT NOT NULL,
			balance INTEGER NOT NULL CHECK (balance >= 0)
		)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('accounts', '{}')`,
		`INSERT INTO accounts (id, holder, balance) VALUES (1,'alice',100),(2,'bob',50)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

func TestTransaction_TransferCommitsBothLegs(t *testing.T) {
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := DataTransaction(ctx, &SQLTransaction{
		Operations: []any{
			map[string]any{
				"action": "update", "table_name": "accounts", "id": float64(1),
				"data": map[string]any{"balance": 70},
			},
			map[string]any{
				"action": "update", "table_name": "accounts", "id": float64(2),
				"data": map[string]any{"balance": 80},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	var aBal, bBal int
	db.QueryRow("SELECT balance FROM accounts WHERE id=1").Scan(&aBal)
	db.QueryRow("SELECT balance FROM accounts WHERE id=2").Scan(&bBal)
	if aBal != 70 || bBal != 80 {
		t.Errorf("expected (70,80), got (%d,%d)", aBal, bBal)
	}
}

func TestTransaction_RollsBackOnSecondLegFailure(t *testing.T) {
	// First update succeeds; second violates the CHECK (balance >= 0). The
	// transaction handler must roll the first leg back so neither row moved.
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataTransaction(ctx, &SQLTransaction{
		Operations: []any{
			map[string]any{
				"action": "update", "table_name": "accounts", "id": float64(1),
				"data": map[string]any{"balance": 70},
			},
			map[string]any{
				"action": "update", "table_name": "accounts", "id": float64(2),
				"data": map[string]any{"balance": -999},
			},
		},
	})
	if res != nil && res.Success {
		t.Fatal("expected failure on CHECK-constraint-violating second leg")
	}
	var aBal, bBal int
	db.QueryRow("SELECT balance FROM accounts WHERE id=1").Scan(&aBal)
	db.QueryRow("SELECT balance FROM accounts WHERE id=2").Scan(&bBal)
	if aBal != 100 || bBal != 50 {
		t.Errorf("expected initial balances preserved (100,50), got (%d,%d), first leg leaked through", aBal, bBal)
	}
}

func TestTransaction_RejectsDisallowedInnerAction(t *testing.T) {
	// query / count / aggregate / search / create_table / etc. are not
	// allowed, the transactional guarantee is meaningless for SELECTs and
	// SQLite implicitly commits before DDL. The handler must say no with an
	// actionable message naming the legal set.
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataTransaction(ctx, &SQLTransaction{
		Operations: []any{
			map[string]any{
				"action": "query", "table_name": "accounts",
			},
		},
	})
	if res.Success {
		t.Fatal("expected failure when inner action is read-only")
	}
	if !strings.Contains(res.Error, "not allowed") || !strings.Contains(res.Error, "insert / update / delete") {
		t.Errorf("error should name the allowed set: %s", res.Error)
	}
}

func TestTransaction_RejectsNestedTransaction(t *testing.T) {
	// A "transaction" inside a transaction is structural nonsense, SQLite
	// has no nested-tx primitive, so the validator must catch it before we
	// open any tx.
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataTransaction(ctx, &SQLTransaction{
		Operations: []any{
			map[string]any{"action": "transaction", "operations": []any{}},
		},
	})
	if res.Success {
		t.Fatal("expected failure on nested transaction")
	}
}

func TestTransaction_RequiresOperations(t *testing.T) {
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataTransaction(ctx, &SQLTransaction{})
	if res.Success || !strings.Contains(res.Error, "operations is required") {
		t.Errorf("expected operations-required error, got %v", res)
	}
}

func TestTransaction_CapsAt100(t *testing.T) {
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	ops := make([]any, 101)
	for i := range ops {
		ops[i] = map[string]any{
			"action": "update", "table_name": "accounts", "id": float64(1),
			"data": map[string]any{"holder": "x"},
		}
	}
	res, _ := DataTransaction(ctx, &SQLTransaction{Operations: ops})
	if res.Success || !strings.Contains(res.Error, "100") {
		t.Errorf("expected 100-cap rejection, got %v", res)
	}
}

func TestTransaction_BulkInsertReusesOuterTx(t *testing.T) {
	// DataInsertBulk normally opens its own BeginTx. When dispatched from a
	// transaction, the outer tx must be reused, opening a nested one would
	// fail on the same single-connection pool. Verify via a bulk insert that
	// commits cleanly under the transaction handler.
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := DataTransaction(ctx, &SQLTransaction{
		Operations: []any{
			map[string]any{
				"action":     "insert",
				"table_name": "accounts",
				"rows": []any{
					map[string]any{"holder": "carol", "balance": 25},
					map[string]any{"holder": "dave", "balance": 75},
				},
			},
			map[string]any{
				"action": "update", "table_name": "accounts", "id": float64(1),
				"data": map[string]any{"balance": 90},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n)
	if n != 4 {
		t.Errorf("expected 4 accounts after bulk insert, got %d", n)
	}
	var aBal int
	db.QueryRow("SELECT balance FROM accounts WHERE id=1").Scan(&aBal)
	if aBal != 90 {
		t.Errorf("expected alice's balance updated to 90, got %d", aBal)
	}
}

func TestTransaction_BulkInsertRollbackOnLaterFailure(t *testing.T) {
	// Bulk insert succeeds, then a later update with an invalid column fails.
	// Both the bulk-inserted rows AND any earlier mutations must roll back.
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataTransaction(ctx, &SQLTransaction{
		Operations: []any{
			map[string]any{
				"action":     "insert",
				"table_name": "accounts",
				"rows": []any{
					map[string]any{"holder": "carol", "balance": 25},
				},
			},
			map[string]any{
				"action": "update", "table_name": "accounts", "id": float64(1),
				"data": map[string]any{"bogus--column": "x"},
			},
		},
	})
	if res != nil && res.Success {
		t.Fatal("expected failure on invalid update column")
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n)
	if n != 2 {
		t.Errorf("expected bulk-inserted row to be rolled back; got %d accounts (started with 2)", n)
	}
}

func TestTransaction_OperationsArgIsScrubbed(t *testing.T) {
	// The handler stashes "_tx" on each per-op map to thread the transaction
	// through. After dispatch, the key must be removed so the caller's
	// original map doesn't leak the tx pointer (the same map is accessible
	// to the agent's tool result rendering).
	db := setupTransferDB(t)
	ctx := &tools.ToolContext{DB: db}
	op := map[string]any{
		"action": "update", "table_name": "accounts", "id": float64(1),
		"data": map[string]any{"balance": 70},
	}
	_, err := DataTransaction(ctx, &SQLTransaction{Operations: []any{op}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, leaked := op["_tx"]; leaked {
		t.Error("_tx leaked into caller's per-op map after dispatch")
	}
}
