package db

import (
	"database/sql"
	"embed"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// expectedCoreTables are the tables the core-DB migrations must create. Kept as
// an explicit list so a migration that silently drops or renames one fails here
// instead of at first run on a user's machine.
var expectedCoreTables = []string{
	"migrations", "users", "settings", "projects",
	"providers", "llm_models", "bot_sessions", "global_connectors", "user_preferences",
	"admin_audit",
}

// TestOpenAppliesMigrationsAndRestartIsIdempotent covers the production path:
// Open() applies every embedded migration to a fresh database, and a second
// Open() (the app restarting against an existing data dir) is a clean no-op -
// no error and no migration re-applied. A non-idempotent bookkeeping bug would
// re-run a migration or duplicate its ledger row; both are caught here.
func TestOpenAppliesMigrationsAndRestartIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")

	d1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open (fresh migrate): %v", err)
	}
	for _, tbl := range expectedCoreTables {
		if !tableExists(t, d1.DB, tbl) {
			t.Errorf("migrations did not create table %q", tbl)
		}
	}
	migCount := migrationRowCount(t, d1.DB)
	if migCount == 0 {
		t.Fatalf("migrations ledger is empty after first Open")
	}
	if err := d1.Close(); err != nil {
		t.Fatalf("close d1: %v", err)
	}

	// Restart: open the same file again. migrate() must see every migration
	// already applied and do nothing - no error, no new ledger rows.
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (restart) must not error: %v", err)
	}
	defer d2.Close()

	if got := migrationRowCount(t, d2.DB); got != migCount {
		t.Errorf("restart re-applied migrations: ledger went from %d to %d rows", migCount, got)
	}
	for _, tbl := range expectedCoreTables {
		if !tableExists(t, d2.DB, tbl) {
			t.Errorf("table %q missing after restart", tbl)
		}
	}
}

// TestCoreMigrationDDLIsIdempotent applies each embedded core migration's raw
// SQL twice to a throwaway database and requires both passes to succeed. The
// bookkeeping ledger normally prevents re-execution, so this guards the
// convention directly: every migration must be safe to re-run (CREATE ... IF
// NOT EXISTS, etc.), which is what makes a crashed or half-applied upgrade
// recoverable instead of a database that refuses to open.
func TestCoreMigrationDDLIsIdempotent(t *testing.T) {
	assertMigrationsReRunnable(t, migrationFS, "migrations")
}

// TestProjectMigrationDDLIsIdempotent is the same guard for the per-project
// database - the one that holds the user's actual application data and is often
// their only copy, so a non-idempotent project migration is the higher-stakes
// data-loss risk.
func TestProjectMigrationDDLIsIdempotent(t *testing.T) {
	assertMigrationsReRunnable(t, projectMigrationFS, "project_migrations")
}

// assertMigrationsReRunnable execs every .sql file under dir (in filename order)
// twice against one fresh in-memory database, failing if any statement errors on
// either pass. Uses the same splitSQLStatements the real migrator uses so the
// split matches production exactly.
func assertMigrationsReRunnable(t *testing.T, fsys embed.FS, dir string) {
	t.Helper()
	sdb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	defer sdb.Close()

	entries, err := fsys.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for pass := 1; pass <= 2; pass++ {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			content, err := fsys.ReadFile(dir + "/" + e.Name())
			if err != nil {
				t.Fatalf("read %s/%s: %v", dir, e.Name(), err)
			}
			for _, stmt := range splitSQLStatements(string(content)) {
				if _, err := sdb.Exec(stmt); err != nil {
					t.Fatalf("pass %d: %s/%s not idempotent - statement failed on re-apply:\n%s\nerror: %v",
						pass, dir, e.Name(), stmt, err)
				}
			}
		}
	}
}

func tableExists(t *testing.T, q *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := q.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("querying sqlite_master for %q: %v", name, err)
	}
	return got == name
}

func migrationRowCount(t *testing.T, q *sql.DB) int {
	t.Helper()
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM migrations`).Scan(&n); err != nil {
		t.Fatalf("count migrations ledger: %v", err)
	}
	return n
}
