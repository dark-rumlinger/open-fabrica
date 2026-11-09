package data

import (
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// setupPostsDB mirrors the blog-build schema: posts.title/slug are NOT NULL with
// no default, body is NOT NULL, the rest are optional/defaulted.
func setupPostsDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	stmts := []string{
		`CREATE TABLE posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			slug TEXT NOT NULL,
			body TEXT NOT NULL,
			tags TEXT,
			published BOOLEAN DEFAULT 0,
			published_at TEXT,
			created_at TEXT DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('posts', '{}')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

func completePost(title, slug, body string) map[string]any {
	return map[string]any{
		"title": title, "slug": slug, "body": body,
		"tags": "go", "published": true, "published_at": "2026-06-02",
	}
}

// TestBulkInsert_TruncatedFirstRowRejectedEarly covers a truncated first row:
// the first row arrives as a partial object (only a clipped body, no title/slug)
// while the rest are complete. The pre-flight must reject before the DB, name
// the missing required columns, and flag the row as truncated.
func TestBulkInsert_TruncatedFirstRowRejectedEarly(t *testing.T) {
	db := setupPostsDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := DataInsert(ctx, &DataActInsert{
		TableName: "posts",
		Rows: []any{
			map[string]any{"body": "# Why I Still Write in Markdown\n\n...no ribbon menus, no "},
			completePost("Go in Ten Minutes", "go-in-ten", "Go's stdlib is powerful."),
			completePost("Thoughts on Architecture", "thoughts", "More thoughts to come."),
		},
	}, nil)
	if err != nil {
		t.Fatalf("expected a clean Result error, not a Go error: %v", err)
	}
	if res.Success {
		t.Fatal("expected the insert to be rejected, but it succeeded")
	}
	msg := res.Error
	for _, want := range []string{"row 0", "title", "slug", "truncated"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q; got: %s", want, msg)
		}
	}
	// Nothing should have been inserted.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM posts").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("no rows should be inserted on a rejected batch; got %d", n)
	}
}

// TestBulkInsert_AllRowsCompleteSucceeds is the corrected retry: every row complete.
func TestBulkInsert_AllRowsCompleteSucceeds(t *testing.T) {
	db := setupPostsDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := DataInsert(ctx, &DataActInsert{
		TableName: "posts",
		Rows: []any{
			completePost("Markdown", "markdown", "Just you and the words."),
			completePost("Go in Ten Minutes", "go-in-ten", "Go's stdlib is powerful."),
		},
	}, nil)
	if err != nil || !res.Success {
		t.Fatalf("complete rows should insert; err=%v res=%+v", err, res)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM posts").Scan(&n)
	if n != 2 {
		t.Errorf("expected 2 rows inserted, got %d", n)
	}
}

// TestBulkInsert_SparseOptionalColumnsNotFlagged guards against false positives:
// rows that legitimately differ only in OPTIONAL columns must still insert.
func TestBulkInsert_SparseOptionalColumnsNotFlagged(t *testing.T) {
	db := setupPostsDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := DataInsert(ctx, &DataActInsert{
		TableName: "posts",
		Rows: []any{
			// All required cols present; one omits optional tags/published_at.
			map[string]any{"title": "A", "slug": "a", "body": "x"},
			completePost("B", "b", "y"),
		},
	}, nil)
	if err != nil || !res.Success {
		t.Fatalf("rows differing only in optional columns must insert; err=%v res=%+v", err, res)
	}
}

// setupOrderItemsDB builds a typical e-commerce schema: order_items FKs to both
// orders and products, a shape where a bad seed fails with an opaque
// "FOREIGN KEY constraint failed (787)".
func setupOrderItemsDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	stmts := []string{
		`CREATE TABLE products (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY AUTOINCREMENT, total REAL)`,
		`CREATE TABLE order_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id INTEGER NOT NULL REFERENCES orders(id),
			product_id INTEGER NOT NULL REFERENCES products(id),
			quantity INTEGER NOT NULL
		)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('order_items','{}')`,
		`INSERT INTO products (id, name) VALUES (1,'a'),(2,'b')`,
		`INSERT INTO orders (id, total) VALUES (1, 9.0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

// TestBulkInsert_DanglingFKRejectedWithDetail pins the dangling-FK improvement: a row
// referencing a non-existent parent is rejected before the DB with the column,
// the offending value, and the parent table named - not an opaque rowid.
func TestBulkInsert_DanglingFKRejectedWithDetail(t *testing.T) {
	db := setupOrderItemsDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := DataInsert(ctx, &DataActInsert{
		TableName: "order_items",
		Rows: []any{
			map[string]any{"order_id": 1, "product_id": 1, "quantity": 2},
			map[string]any{"order_id": 1, "product_id": 99, "quantity": 1}, // 99 doesn't exist
		},
	}, nil)
	if err != nil {
		t.Fatalf("expected a clean Result error, not a Go error: %v", err)
	}
	if res.Success {
		t.Fatal("expected rejection for the dangling FK")
	}
	for _, want := range []string{"row 1", "product_id", "99", "products"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error should mention %q; got: %s", want, res.Error)
		}
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM order_items").Scan(&n)
	if n != 0 {
		t.Errorf("no rows should be inserted on a rejected batch; got %d", n)
	}
}

// TestBulkInsert_ValidFKsSucceed guards against false positives.
func TestBulkInsert_ValidFKsSucceed(t *testing.T) {
	db := setupOrderItemsDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := DataInsert(ctx, &DataActInsert{
		TableName: "order_items",
		Rows: []any{
			map[string]any{"order_id": 1, "product_id": 1, "quantity": 2},
			map[string]any{"order_id": 1, "product_id": 2, "quantity": 3},
		},
	}, nil)
	if err != nil || !res.Success {
		t.Fatalf("valid FKs should insert; err=%v res=%+v", err, res)
	}
}

// A misspelled column in a seed array is named, with the closest real column, before
// anything is written.
func TestPreflightUnknownColumns(t *testing.T) {
	db := tooltest.ProjectDB(t)
	if _, err := db.Exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, order_id TEXT, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows := []any{
		map[string]any{"order_id": "a", "status": "new"},
		map[string]any{"orderorder_id": "b", "status": "new"},
	}
	msg := preflightUnknownColumns(db, "orders", rows)
	if !strings.Contains(msg, `"orderorder_id"`) || !strings.Contains(msg, "order_id") || !strings.Contains(msg, "row 2") || !strings.Contains(msg, "nothing was inserted") {
		t.Fatalf("message = %q", msg)
	}
	if got := preflightUnknownColumns(db, "orders", rows[:1]); got != "" {
		t.Fatalf("valid rows must pass, got %q", got)
	}
}
