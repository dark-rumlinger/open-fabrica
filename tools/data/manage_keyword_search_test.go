package data

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupSearchDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	stmts := []string{
		`CREATE TABLE articles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT,
			body TEXT
		)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('articles', '{}')`,
		`INSERT INTO articles (title, body) VALUES
			('Sourdough basics', 'how to start a sourdough starter'),
			('Bread crumbs', 'using stale bread')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

// TestSearchCreateIndexAndQueryRoundTrip pins the FTS round trip: create_index
// (manage_keyword_search) must forward table_name + columns to build the index, and
// the query (manage_data action=search, which delegates to the one searchQuery
// implementation) must return matches. A dropped columns arg would yield no index.
func TestSearchCreateIndexAndQueryRoundTrip(t *testing.T) {
	db := setupSearchDB(t)
	ctx := &tools.ToolContext{DB: db}

	create := &SearchCreateIndex{
		TableName: "articles",
		Columns:   []any{"title", "body"},
	}
	res, err := create.Run(ctx)
	if err != nil || res == nil || !res.Success {
		t.Fatalf("create_index: err=%v res=%+v", err, res)
	}
	var ftsCount int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='articles_fts'").Scan(&ftsCount)
	if ftsCount != 1 {
		t.Fatalf("columns arg dropped: FTS index not created")
	}

	q := &DataActSearch{TableName: "articles", QueryText: "sourdough", Limit: 5}
	qres, err := q.Run(ctx)
	if err != nil || qres == nil || !qres.Success {
		t.Fatalf("search: err=%v res=%+v", err, qres)
	}
	data := qres.Data.(map[string]any)
	if data["query"] != "sourdough" {
		t.Errorf("fts_query dropped: %+v", data["query"])
	}
	if c, _ := data["count"].(int); c != 1 {
		t.Errorf("expected 1 match for 'sourdough', got %v", data["count"])
	}
}

// TestSearchSurfacesDBError verifies that a real DB fault on an existence-check
// query is reported as a query failure, not misreported as "table/index not
// found" (which would steer a caller down the wrong repair path). We force the
// fault by handing the handlers an already-closed DB.
func TestSearchSurfacesDBError(t *testing.T) {
	// Schema is irrelevant once closed: every query now errors with "database is closed".
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	ctx := &tools.ToolContext{DB: db}

	// create_index: the of_dynamic_tables existence check must surface the error.
	cres, _ := (&SearchCreateIndex{TableName: "articles", Columns: []any{"title"}}).Run(ctx)
	if cres == nil || cres.Success {
		t.Fatalf("create_index: expected failure on closed DB, got %+v", cres)
	}
	if !strings.Contains(cres.Error, "check failed") {
		t.Errorf("create_index: expected a query-failure error, got %q", cres.Error)
	}

	// search: the FTS existence check must surface the error rather than claim
	// "no FTS index".
	qres, _ := (&DataActSearch{TableName: "articles", QueryText: "sourdough", Limit: 5}).Run(ctx)
	if qres == nil || qres.Success {
		t.Fatalf("search: expected failure on closed DB, got %+v", qres)
	}
	if !strings.Contains(qres.Error, "check failed") {
		t.Errorf("search: expected a query-failure error, got %q (must not be a bare not-found)", qres.Error)
	}
}

// TestSearchEmbedSurfacesWriteFailure verifies that when persisting an embedding
// fails, embed reports a failure rather than claiming rows were embedded. We let
// the row read succeed but block the _embedding UPDATE with an aborting trigger,
// so a regression that swallows the write error would falsely report success.
func TestSearchEmbedSurfacesWriteFailure(t *testing.T) {
	db := setupSearchDB(t)
	ctx := &tools.ToolContext{
		DB: db,
		EmbedFunc: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range texts {
				out[i] = []float32{0.1, 0.2, 0.3}
			}
			return out, nil
		},
	}

	if _, err := (&SearchCreateVectorIndex{TableName: "articles", EmbeddingColumns: []any{"title", "body"}}).Run(ctx); err != nil {
		t.Fatalf("create_vector_index: %v", err)
	}

	// Block writes to _embedding so the row SELECT still succeeds but the UPDATE
	// that stores each vector fails.
	if _, err := db.Exec(`CREATE TRIGGER block_embed BEFORE UPDATE OF _embedding ON articles
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	res, err := (&SearchEmbed{TableName: "articles"}).Run(ctx)
	if err != nil {
		t.Fatalf("embed returned transport error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("embed must fail when the _embedding write fails, got %+v", res)
	}
	if !strings.Contains(res.Error, "storing embedding") {
		t.Errorf("expected a storing-embedding error, got %q", res.Error)
	}
}

// Search results are stripped of PASSWORD columns before they leave the tool -
// they land in the model's context and in the persisted build transcript. That
// strip must fail CLOSED: if the secure-column projection can't be read, the
// code cannot know which columns are secret, so it must refuse rather than
// return rows it could not sanitize. Unlike the admin CRUD handlers (where
// LoadTableSchema reads the same registry first and 404s), nothing upstream here
// touches of_dynamic_tables - the FTS checks hit sqlite_master and the data
// table - so an unreadable projection reaches the strip with rows in hand.
func TestSearchQueryFailsClosedWhenSecureColumnsUnreadable(t *testing.T) {
	db := setupSearchDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := db.Exec(`ALTER TABLE articles ADD COLUMN password TEXT`); err != nil {
		t.Fatalf("add column: %v", err)
	}
	if _, err := db.Exec(`UPDATE articles SET password = 'secret-hash'`); err != nil {
		t.Fatalf("seed password: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE of_dynamic_tables SET secure_columns = '{"password":"hash"}' WHERE table_name = 'articles'`); err != nil {
		t.Fatalf("register secure column: %v", err)
	}
	if _, err := (&SearchCreateIndex{TableName: "articles", Columns: []any{"title", "body"}}).Run(ctx); err != nil {
		t.Fatalf("create_index: %v", err)
	}

	// Force the projection read to fail the way a corrupt/unavailable database
	// would. The FTS index and the data table stay intact, so the search itself
	// still succeeds and returns the password column.
	if _, err := db.Exec(`DROP TABLE of_dynamic_tables`); err != nil {
		t.Fatalf("drop registry: %v", err)
	}

	qres, err := (&DataActSearch{TableName: "articles", QueryText: "sourdough", Limit: 5}).Run(ctx)
	if err != nil {
		t.Fatalf("search returned transport error: %v", err)
	}
	if qres == nil {
		t.Fatal("search returned no result")
	}
	// Assert the outcome that matters rather than the mechanism: whether it
	// refuses or returns sanitized rows, the hash must not escape.
	if qres.Success {
		data, _ := qres.Data.(map[string]any)
		results, _ := data["results"].([]map[string]any)
		for _, row := range results {
			if _, leaked := row["password"]; leaked {
				t.Fatalf("search returned a PASSWORD column when the secure-column projection was unreadable: %+v", row)
			}
		}
	}
}

// TestSearchListAndDropRoundTrip pins list_indexes and drop_index forwarding.
func TestSearchListAndDropRoundTrip(t *testing.T) {
	db := setupSearchDB(t)
	ctx := &tools.ToolContext{DB: db}

	if _, err := (&SearchCreateIndex{TableName: "articles", Columns: []any{"title"}}).Run(ctx); err != nil {
		t.Fatalf("create_index: %v", err)
	}

	listed, err := (&SearchListIndexes{}).Run(ctx)
	if err != nil || listed == nil || !listed.Success {
		t.Fatalf("list_indexes: err=%v res=%+v", err, listed)
	}

	drop := &SearchDropIndex{TableName: "articles"}
	dres, err := drop.Run(ctx)
	if err != nil || dres == nil || !dres.Success {
		t.Fatalf("drop_index: err=%v res=%+v", err, dres)
	}
	var ftsCount int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='articles_fts'").Scan(&ftsCount)
	if ftsCount != 0 {
		t.Errorf("drop_index table_name dropped: FTS index still present")
	}
}
