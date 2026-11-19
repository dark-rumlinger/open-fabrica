package admin

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/db"
)

// migratedProjectDBPath creates a real, fully migrated project database and
// returns its path with every handle closed, for code that opens the file itself.
func migratedProjectDBPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mgr := db.NewProjectDBManager(dir)
	if _, err := mgr.Create(1); err != nil {
		t.Fatalf("create project db: %v", err)
	}
	mgr.CloseAll()
	return filepath.Join(mgr.ProjectDir(1), "project.db")
}

// insertRow inserts one row into a platform table, giving every required column
// the caller did not set a placeholder value, so a test names only what it asserts on.
func insertRow(t *testing.T, d *sql.DB, table string, set map[string]any) {
	t.Helper()
	rows, err := d.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	type column struct {
		name, typ string
		notNull   bool
		hasDflt   bool
		pk        bool
	}
	var table_ []column
	pkCols := 0
	for rows.Next() {
		var c column
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&c.name, &c.typ, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		c.notNull, c.hasDflt, c.pk = notNull != 0, dflt.Valid, pk > 0
		if c.pk {
			pkCols++
		}
		table_ = append(table_, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("table_info(%s) rows: %v", table, err)
	}
	rows.Close()

	var cols []string
	var vals []any
	seen := map[string]bool{}
	for _, c := range table_ {
		seen[c.name] = true
		if v, ok := set[c.name]; ok {
			cols, vals = append(cols, c.name), append(vals, v)
			continue
		}
		typ := strings.ToUpper(c.typ)
		integer := strings.Contains(typ, "INT")
		// A lone integer primary key is the rowid: SQLite assigns it.
		rowid := c.pk && pkCols == 1 && integer
		if c.hasDflt || rowid || !(c.notNull || c.pk) {
			continue
		}
		cols = append(cols, c.name)
		if integer || strings.Contains(typ, "BOOL") {
			vals = append(vals, 1)
		} else {
			vals = append(vals, "x")
		}
	}
	if len(seen) == 0 {
		t.Fatalf("table %s does not exist in the migrated schema", table)
	}
	for name := range set {
		if !seen[name] {
			t.Fatalf("table %s has no column %q", table, name)
		}
	}
	stmt := "INSERT INTO " + table + " DEFAULT VALUES"
	if len(cols) > 0 {
		stmt = "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + ")"
	}
	if _, err := d.Exec(stmt, vals...); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}

// rowCount counts the rows of a table.
func rowCount(t *testing.T, d *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
