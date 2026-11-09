package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	"openfabrica/db"
	sdeps "openfabrica/server/deps"

	_ "modernc.org/sqlite"
)

// seedRowsTable creates a small dynamic table "items" with four rows, including
// one whose name carries a literal '%', so the LIKE-escaping path is exercised.
func seedRowsTable(t *testing.T) *db.ProjectDBManager {
	t.Helper()
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := pdb.ExecWrite(`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT, qty INTEGER)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	if _, err := pdb.ExecWrite(
		`INSERT INTO of_dynamic_tables (table_name, schema_def, secure_columns) VALUES (?, ?, ?)`,
		"items", `{"id":"INTEGER","name":"TEXT","qty":"INTEGER"}`, `{}`); err != nil {
		t.Fatalf("register table: %v", err)
	}
	seed := []struct {
		name string
		qty  int
	}{{"apple", 5}, {"banana", 2}, {"cherry", 9}, {"date 50%", 1}}
	for _, s := range seed {
		if _, err := pdb.ExecWrite(`INSERT INTO items (name, qty) VALUES (?, ?)`, s.name, s.qty); err != nil {
			t.Fatalf("insert row: %v", err)
		}
	}
	return mgr
}

func callRows(t *testing.T, mgr *db.ProjectDBManager, query string) map[string]any {
	t.Helper()
	h := &ProjectTablesHandler{deps: &Deps{Core: sdeps.Core{ProjectDBManager: mgr, Logger: slog.Default()}}}
	req := httptest.NewRequest("GET", "/?"+query, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("projectID", "1")
	rctx.URLParams.Add("tableName", "items")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.Rows(w, req)
	if w.Code != 200 {
		t.Fatalf("Rows %q: status %d body %s", query, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", query, err)
	}
	return out
}

func rowNames(out map[string]any) []string {
	rows, _ := out["rows"].([]any)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			if s, ok := m["name"].(string); ok {
				names = append(names, s)
			}
		}
	}
	return names
}

func TestRowsSortByColumn(t *testing.T) {
	mgr := seedRowsTable(t)

	if got := rowNames(callRows(t, mgr, "sort=name&dir=asc")); !slices.Equal(got, []string{"apple", "banana", "cherry", "date 50%"}) {
		t.Errorf("sort=name asc: got %v", got)
	}
	if got := rowNames(callRows(t, mgr, "sort=qty&dir=desc")); !slices.Equal(got, []string{"cherry", "apple", "banana", "date 50%"}) {
		t.Errorf("sort=qty desc: got %v", got)
	}
}

// An unknown or malicious sort column must fall back to the default id-desc order
// and must never reach the query as SQL.
func TestRowsUnknownSortFallsBackSafely(t *testing.T) {
	mgr := seedRowsTable(t)
	out := callRows(t, mgr, "sort="+url.QueryEscape("bogus; DROP TABLE items")+"&dir=asc")
	if got := rowNames(out); !slices.Equal(got, []string{"date 50%", "cherry", "banana", "apple"}) {
		t.Errorf("unknown sort should be id desc, got %v", got)
	}
	// The table must still be intact (the injection attempt did nothing).
	if total, _ := out["total"].(float64); total != 4 {
		t.Errorf("expected 4 rows intact, got total %v", total)
	}
}

func TestRowsSearchFilters(t *testing.T) {
	mgr := seedRowsTable(t)
	out := callRows(t, mgr, "q=an")
	if got := rowNames(out); !slices.Equal(got, []string{"banana"}) {
		t.Errorf("q=an should match only banana, got %v", got)
	}
	if total, _ := out["total"].(float64); total != 1 {
		t.Errorf("filtered total should be 1, got %v", total)
	}
}

// A '%' in the query is a literal to search for, not a LIKE wildcard: it must
// match only the one row that actually contains it.
func TestRowsSearchWildcardIsLiteral(t *testing.T) {
	mgr := seedRowsTable(t)
	out := callRows(t, mgr, "q="+url.QueryEscape("50%"))
	if got := rowNames(out); !slices.Equal(got, []string{"date 50%"}) {
		t.Errorf(`q=50%% should match only "date 50%%", got %v`, got)
	}
}
