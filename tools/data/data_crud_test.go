package data

import (
	"database/sql"
	"strings"
	"testing"

	"openfabrica/internal/dbh"
	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

func setupDataDB(t *testing.T) dbh.Handle {
	t.Helper()
	db := tooltest.ProjectDB(t)
	stmts := []string{
		`CREATE TABLE models (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			slug TEXT,
			name TEXT,
			cover_image TEXT,
			featured INTEGER DEFAULT 0
		)`,
		`INSERT INTO of_dynamic_tables (table_name, schema_def) VALUES ('models', '{}')`,
		`INSERT INTO models (slug, name) VALUES ('amara','Amara'),('sven','Sven'),('ines','Ines')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return db
}

func TestDataUpdate_ByFiltersUpdatesMatchingRows(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := DataUpdate(ctx, &DataActUpdate{
		TableName: "models",
		Data:      map[string]any{"cover_image": "/files/amara-1.jpg"},
		Filters: []any{
			map[string]any{"column": "slug", "op": "=", "value": "amara"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	if n := res.Data.(map[string]any)["rows_affected"].(int64); n != 1 {
		t.Errorf("expected 1 row affected, got %d", n)
	}
	var img string
	db.QueryRow("SELECT cover_image FROM models WHERE slug='amara'").Scan(&img)
	if img != "/files/amara-1.jpg" {
		t.Errorf("expected cover_image to be backfilled, got %q", img)
	}
}

// update and search report their two missing-arg cases distinctly - a missing
// name says so, and a present name with an empty data/query_text names the table
// and the missing piece - rather than a single "table_name and X are required"
// string that reads (in chat) as if the name were missing. Mirrors create_table's
// handling of the empty-column_defs shape.
func TestDataUpdate_DistinctRequiredMessages(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, _ := DataUpdate(ctx, &DataActUpdate{
		Data: map[string]any{"name": "X"},
	}, nil); res.Success || !strings.Contains(res.Error, "update requires a table_name") {
		t.Errorf("a missing table_name must say so, got %+v", res)
	}

	res, _ := DataUpdate(ctx, &DataActUpdate{
		TableName: "models",
		Data:      map[string]any{},
	}, nil)
	if res.Success || !strings.Contains(res.Error, "models") || !strings.Contains(res.Error, "data") {
		t.Errorf("an empty data object must name the table and the missing piece, got: %s", res.Error)
	}
	if strings.Contains(res.Error, "table_name and data are required") {
		t.Errorf("must not fall back to the both-params message")
	}
}

func TestDataSearch_DistinctRequiredMessages(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, _ := DataSearch(ctx, &DataActSearch{QueryText: "hi"}); res.Success ||
		!strings.Contains(res.Error, "search requires a table_name") {
		t.Errorf("a missing table_name must say so, got %+v", res)
	}
	res, _ := DataSearch(ctx, &DataActSearch{TableName: "models"})
	if res.Success || !strings.Contains(res.Error, "models") || !strings.Contains(res.Error, "query_text") {
		t.Errorf("a missing query_text must name the table and the missing piece, got: %s", res.Error)
	}
}

func TestDataUpdate_RejectsBothIdAndFilters(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataUpdate(ctx, &DataActUpdate{
		TableName: "models",
		ID:        1,
		Data:      map[string]any{"name": "X"},
		Filters: []any{
			map[string]any{"column": "slug", "op": "=", "value": "amara"},
		},
	}, nil)
	if res.Success {
		t.Fatal("expected error when both id and filters provided")
	}
	if !strings.Contains(res.Error, "either id or filters") {
		t.Errorf("error should mention id-or-filters: %s", res.Error)
	}
}

func TestDataUpdate_RejectsNeither(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataUpdate(ctx, &DataActUpdate{
		TableName: "models",
		Data:      map[string]any{"name": "X"},
	}, nil)
	if res.Success {
		t.Fatal("expected error when neither id nor filters provided")
	}
}

func TestDataBatchUpdate_AllEntriesAppliedAtomically(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, err := DataBatchUpdate(ctx, &DataActBatchUpdate{
		TableName: "models",
		Updates: []any{
			map[string]any{
				"filters": []any{map[string]any{"column": "slug", "op": "=", "value": "amara"}},
				"data":    map[string]any{"cover_image": "/files/amara.jpg"},
			},
			map[string]any{
				"filters": []any{map[string]any{"column": "slug", "op": "=", "value": "sven"}},
				"data":    map[string]any{"cover_image": "/files/sven.jpg"},
			},
			map[string]any{
				"filters": []any{map[string]any{"column": "slug", "op": "=", "value": "ines"}},
				"data":    map[string]any{"cover_image": "/files/ines.jpg"},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success: %s", res.Error)
	}
	d := res.Data.(map[string]any)
	if d["updated"].(int64) != 3 {
		t.Errorf("expected updated=3, got %v", d["updated"])
	}
	if d["total_rows_affected"].(int64) != 3 {
		t.Errorf("expected total_rows_affected=3, got %v", d["total_rows_affected"])
	}
	rows, _ := db.Query("SELECT slug, cover_image FROM models ORDER BY slug")
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var s, c string
		rows.Scan(&s, &c)
		got[s] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating rows: %v", err)
	}
	want := map[string]string{
		"amara": "/files/amara.jpg",
		"ines":  "/files/ines.jpg",
		"sven":  "/files/sven.jpg",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("slug %q: expected cover_image %q, got %q", k, v, got[k])
		}
	}
}

func TestDataBatchUpdate_SurfacesZeroMatchPerEntry(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataBatchUpdate(ctx, &DataActBatchUpdate{
		TableName: "models",
		Updates: []any{
			map[string]any{
				"filters": []any{map[string]any{"column": "slug", "op": "=", "value": "amara"}},
				"data":    map[string]any{"cover_image": "/files/x.jpg"},
			},
			map[string]any{
				"filters": []any{map[string]any{"column": "slug", "op": "=", "value": "ghost"}},
				"data":    map[string]any{"cover_image": "/files/ghost.jpg"},
			},
		},
	})
	if !res.Success {
		t.Fatalf("expected success even with zero-match entry: %s", res.Error)
	}
	d := res.Data.(map[string]any)
	results := d["results"].([]map[string]any)
	if len(results) != 2 {
		t.Fatalf("expected 2 per-entry results, got %d", len(results))
	}
	if results[0]["rows_affected"].(int64) != 1 {
		t.Errorf("entry 0 expected rows_affected=1, got %v", results[0]["rows_affected"])
	}
	if results[1]["rows_affected"].(int64) != 0 {
		t.Errorf("entry 1 expected rows_affected=0, got %v", results[1]["rows_affected"])
	}
	if d["total_rows_affected"].(int64) != 1 {
		t.Errorf("expected total_rows_affected=1, got %v", d["total_rows_affected"])
	}
}

func TestDataBatchUpdate_RollsBackOnInvalidEntry(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	res, _ := DataBatchUpdate(ctx, &DataActBatchUpdate{
		TableName: "models",
		Updates: []any{
			map[string]any{
				"filters": []any{map[string]any{"column": "slug", "op": "=", "value": "amara"}},
				"data":    map[string]any{"cover_image": "/files/amara.jpg"},
			},
			map[string]any{
				"filters": []any{map[string]any{"column": "slug", "op": "=", "value": "sven"}},
				"data":    map[string]any{"bogus--col": "x"},
			},
		},
	})
	if res.Success {
		t.Fatal("expected failure on invalid column in second entry")
	}
	var img sql.NullString
	db.QueryRow("SELECT cover_image FROM models WHERE slug='amara'").Scan(&img)
	if img.Valid && img.String == "/files/amara.jpg" {
		t.Errorf("expected first entry rolled back, but cover_image = %q", img.String)
	}
}

func TestDataBatchUpdate_CapsAt100(t *testing.T) {
	db := setupDataDB(t)
	ctx := &tools.ToolContext{DB: db}
	updates := make([]any, 101)
	for i := range updates {
		updates[i] = map[string]any{
			"id":   float64(1),
			"data": map[string]any{"name": "x"},
		}
	}
	res, _ := DataBatchUpdate(ctx, &DataActBatchUpdate{
		TableName: "models",
		Updates:   updates,
	})
	if res.Success {
		t.Fatal("expected failure at 101 entries")
	}
	if !strings.Contains(res.Error, "100") {
		t.Errorf("error should mention 100 cap: %s", res.Error)
	}
}
