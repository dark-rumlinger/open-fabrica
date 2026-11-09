package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawIgnoredExec matches an assignment that discards the result of a .Exec call,
// e.g. `_, _ = db.Exec(...)` or `_ = tx.Exec(...)`. Such writes skip busy-retry
// and error logging, so a failure silently drifts accounting tables under
// contention instead of surfacing.
var rawIgnoredExec = regexp.MustCompile(`^\s*_(?:,\s*_)?\s*=\s*[\w.]+\.Exec\(`)

// execGuardDirs are the package directories whose write discipline this guard
// enforces, relative to the store package directory (the test's working dir).
var execGuardDirs = []string{".", "../db"}

// TestNoRawIgnoredExec fails if a non-test file in store/ or db/ discards a
// .Exec result: route best-effort writes through sqliteretry.ExecHandle (or
// store.bestEffortExec) so a failed write retries on busy and logs instead of
// vanishing. Add the marker "exec-guard:allow" on the line to opt an intentional discard out.
func TestNoRawIgnoredExec(t *testing.T) {
	for _, dir := range execGuardDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "exec-guard:allow") {
					continue
				}
				if rawIgnoredExec.MatchString(line) {
					t.Errorf("%s:%d discards a .Exec result; route it through sqliteretry/bestEffortExec: %s",
						path, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}
