package models

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
)

// The reflection helpers in scan.go derive both the SELECT list and the Scan
// targets from each model's `db:"col"` tags, so the tags are the only thing
// standing between the struct and the SQL. These tests pin the tags to the real
// migrated schema and pin the two derivations to each other:
//
//   - every db tag names a column that exists, and every column is read by some
//     field (TestDerivedColumnsMatchSchema);
//   - dbScanTargets hands back &field in dbColumns order, so row.Scan writes each
//     column into the field that names it (TestScanTargetsAddressFields).
//
// The schema comes from db.Open, which applies the migrations the same way the
// app does at boot - a migration that adds a column without a struct field
// following (or vice versa) fails here rather than at a user's first query.

// migratedSchema opens a throwaway core DB, applies every migration through the
// production path, and returns the named table's columns in ordinal order.
func migratedSchema(t *testing.T, table string) []string {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatalf("open+migrate core db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	rows, err := d.DB.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name for %s: %v", table, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns of %s: %v", table, err)
	}
	if len(cols) == 0 {
		t.Fatalf("table %q has no columns after migration - the schema pin would pass vacuously", table)
	}
	return cols
}

// TestDerivedColumnsMatchSchema pins each model's db tags to the columns its
// table actually has. A tag naming a missing column makes every SELECT built
// from dbColumns fail at runtime; a column no tag names is silently never read,
// so a column added by a migration never reaches the struct.
func TestDerivedColumnsMatchSchema(t *testing.T) {
	cases := []struct {
		table string
		model any
	}{
		{"providers", LLMProvider{}},
		{"llm_models", LLMModel{}},
	}
	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			schema := migratedSchema(t, c.table)
			tagged := dbColumnNames(c.model)

			inSchema := map[string]bool{}
			for _, col := range schema {
				inSchema[col] = true
			}
			inStruct := map[string]bool{}
			for _, col := range tagged {
				inStruct[col] = true
			}

			for _, col := range tagged {
				if !inSchema[col] {
					t.Errorf("%T has db tag %q, but table %q has no such column - every query built from dbColumns will fail", c.model, col, c.table)
				}
			}
			for _, col := range schema {
				if !inStruct[col] {
					t.Errorf("table %q has column %q that no %T field is tagged for - it is never read; add a db-tagged field or drop the column", c.table, col, c.model)
				}
			}
		})
	}
}

// TestDerivedColumnRenderings pins the two string renderings to the derived name
// list, so dbColumns and dbColumnsAlias cannot drift from each other or from the
// tags.
func TestDerivedColumnRenderings(t *testing.T) {
	names := dbColumnNames(LLMProvider{})

	if got, want := dbColumns(LLMProvider{}), strings.Join(names, ", "); got != want {
		t.Errorf("dbColumns = %q, want %q", got, want)
	}

	aliased := make([]string, len(names))
	for i, n := range names {
		aliased[i] = "p." + n
	}
	if got, want := dbColumnsAlias(LLMProvider{}, "p"), strings.Join(aliased, ", "); got != want {
		t.Errorf("dbColumnsAlias = %q, want %q", got, want)
	}
}

// TestScanTargetsAddressFields verifies dbScanTargets hands back pointers to the
// struct's own fields in dbColumns order. Order is the whole invariant: Scan
// assigns positionally, so a target list that drifts from the column list writes
// each value into the wrong field with no error anywhere.
func TestScanTargetsAddressFields(t *testing.T) {
	var p LLMProvider
	cols := dbColumnNames(p)
	targets := dbScanTargets(&p)
	if len(targets) != len(cols) {
		t.Fatalf("provider scan targets = %d, columns = %d - Scan would misalign", len(targets), len(cols))
	}

	// Anchor both ends to the fields their tags name, so a reordered struct that
	// still has the right count is caught.
	byName := map[string]any{}
	for i, col := range cols {
		byName[col] = targets[i]
	}
	if got, ok := byName["id"].(*int); !ok || got != &p.ID {
		t.Errorf("target for column \"id\" is not &p.ID")
	}
	if got, ok := byName["updated_at"].(*time.Time); !ok || got != &p.UpdatedAt {
		t.Errorf("target for column \"updated_at\" is not &p.UpdatedAt")
	}
	// api_key_encrypted is nullable TEXT backed by *string, so its target must be
	// **string or a NULL key fails to scan.
	if _, ok := byName["api_key_encrypted"].(**string); !ok {
		t.Errorf("target for column \"api_key_encrypted\" is %T, want **string - a NULL key would fail to scan", byName["api_key_encrypted"])
	}

	var m LLMModel
	if got, want := len(dbScanTargets(&m)), len(dbColumnNames(m)); got != want {
		t.Fatalf("model scan targets = %d, columns = %d - Scan would misalign", got, want)
	}
}
