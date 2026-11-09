package data

import (
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/tools"
)

// The other data tests run against an in-memory *sql.DB with no connection cap,
// so a handler that issues a second ctx.DB query while a transaction is open
// still succeeds there. Production uses the per-project writer pool, which is
// capped at a single connection (SetMaxOpenConns(1)); on that pool the second
// query blocks forever waiting for the connection the transaction holds. These
// tests exercise the handlers through the real ProjectDBManager writer pool so
// that class of deadlock fails the suite instead of only production.

// runWithDeadlineGuard runs fn in a goroutine and fails the test if it does not
// return within d, turning a writer-pool self-deadlock into a fast, clear
// failure rather than a hang until the global test timeout.
func runWithDeadlineGuard(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s: likely a writer-pool deadlock", name, d)
	}
}

// setupProjectWriterDB opens a real per-project database (running the project
// migrations) so the writer pool carries the production single-connection cap.
func setupProjectWriterDB(t *testing.T) *db.ProjectDB {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(`CREATE TABLE widgets (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, qty INTEGER)`); err != nil {
		t.Fatalf("create widgets table: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_dynamic_tables (table_name, schema_def, validation) VALUES ('widgets', '{}', '{}')`); err != nil {
		t.Fatalf("register widgets: %v", err)
	}
	return pdb
}

// TestImportJSON_NoDeadlockOnRealWriterPool guards the import_json fix: the
// handler opens a transaction on the writer pool and must load validation rules
// before doing so, not with a second ctx.DB query inside the transaction.
func TestImportJSON_NoDeadlockOnRealWriterPool(t *testing.T) {
	pdb := setupProjectWriterDB(t)
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	var res *tools.Result
	var err error
	runWithDeadlineGuard(t, 10*time.Second, "import_json", func() {
		res, err = DataImportJSON(ctx, &SQLImportJSON{
			TableName: "widgets",
			JSONData:  `[{"name":"a","qty":1},{"name":"b","qty":2}]`,
		})
	})
	if err != nil {
		t.Fatalf("import_json error: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("import_json failed: %+v", res)
	}
	var n int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM widgets").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 imported rows, got %d", n)
	}
}

// TestTransactionDelete_NoDeadlockOnRealWriterPool guards the delete-in-
// transaction fix: a delete dispatched inside a transaction holds the writer
// connection, so its destructive-action audit write must compose into the same
// transaction instead of asking the single-connection pool for another.
func TestTransactionDelete_NoDeadlockOnRealWriterPool(t *testing.T) {
	pdb := setupProjectWriterDB(t)
	if _, err := pdb.ExecWrite(`INSERT INTO widgets (id, name, qty) VALUES (1, 'doomed', 3)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	ctx := &tools.ToolContext{DB: pdb.Writer()}

	var res *tools.Result
	var err error
	runWithDeadlineGuard(t, 10*time.Second, "transaction delete", func() {
		res, err = DataTransaction(ctx, &SQLTransaction{
			Operations: []any{
				map[string]any{"action": "delete", "table_name": "widgets", "id": float64(1)},
			},
		})
	})
	if err != nil {
		t.Fatalf("transaction error: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("transaction failed: %+v", res)
	}
	var remaining int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM widgets WHERE id = 1").Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("row should be deleted, still present")
	}
	// The audit row must have been written through the transaction.
	var logged int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_run_log WHERE kind='tool' AND ref='manage_data'").Scan(&logged); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if logged == 0 {
		t.Errorf("expected a destructive-action audit row in of_run_log")
	}
}
