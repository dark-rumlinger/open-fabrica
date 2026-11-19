package archtest

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "openfabrica/tools/all"
)

// repoRoot is the module root, found from this file's own location so a guard
// reads the sources the same way whatever the working directory is.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// clip collapses whitespace and shortens s to n bytes for a failure message.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
