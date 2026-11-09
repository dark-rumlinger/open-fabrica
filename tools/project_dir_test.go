package tools_test

import (
	"testing"

	"openfabrica/db"
	"openfabrica/tools"
)

// Tools write disk files under ToolContext.ProjectDir and the servers read them
// back through ProjectDBManager.ProjectDir: the two must name the same directory.
func TestToolProjectDirMatchesDBManager(t *testing.T) {
	dir := t.TempDir()
	tc := &tools.ToolContext{DataDir: dir, ProjectID: 42}
	if got, want := tc.ProjectDir(), db.NewProjectDBManager(dir).ProjectDir(42); got != want {
		t.Fatalf("ToolContext.ProjectDir = %q, ProjectDBManager.ProjectDir = %q", got, want)
	}
}
