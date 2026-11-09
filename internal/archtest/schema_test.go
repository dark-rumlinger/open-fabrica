package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var createTable = regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?["\x60]?(\w+)`)

// TestMigrationsOwnTheSchema: a table the migrations define is never also created
// from Go. The migration runs first on every open, so a runtime CREATE TABLE IF NOT
// EXISTS for the same table silently never applies, and any column it adds goes
// missing in production while code relies on it. That is how of_jobs lost the
// result/error columns the job worker writes, leaving failed jobs stuck in
// 'running'. Schema changes go in db/project_migrations (or db/migrations).
func TestMigrationsOwnTheSchema(t *testing.T) {
	root := filepath.Join("..", "..")
	owned := map[string]string{}
	for _, dir := range []string{"db/migrations", "db/project_migrations"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(root, dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range createTable.FindAllStringSubmatch(string(body), -1) {
				owned[strings.ToLower(m[1])] = dir + "/" + e.Name()
			}
		}
	}
	// The runner creates its own bookkeeping table before any migration can run.
	delete(owned, "migrations")
	if len(owned) < 20 {
		t.Fatalf("found only %d migration tables; the scan is broken", len(owned))
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "data", "web", "node_modules", "testdata", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range createTable.FindAllStringSubmatch(string(body), -1) {
			if src, ok := owned[strings.ToLower(m[1])]; ok {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s creates table %s, which %s already defines: change the migration instead", filepath.ToSlash(rel), m[1], src)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
