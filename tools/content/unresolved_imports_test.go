package content

import (
	"testing"

	"openfabrica/analysis"
)

// Characterization tests for the pure helpers in unresolved_imports.go, the
// JS cross-module import/export resolution gate. These pin the small building
// blocks (module detection, overlay, issue de-duplication, base-name) before any
// refactor of this 895-line file.

func TestWithOverlay(t *testing.T) {
	base := map[string]string{"a.js": "1", "b.js": "2"}
	got := withOverlay(base, "b.js", "NEW")
	if got["b.js"] != "NEW" || got["a.js"] != "1" {
		t.Errorf("withOverlay did not overlay correctly: %v", got)
	}
	// Overlay must not mutate the input map.
	if base["b.js"] != "2" {
		t.Errorf("withOverlay mutated the source map: %v", base)
	}
	// A new path is added.
	if got := withOverlay(base, "c.js", "3"); got["c.js"] != "3" {
		t.Errorf("withOverlay did not add new path")
	}
}

func TestIssueKeyIndexAndFilter(t *testing.T) {
	a := analysis.ImportIssue{Reason: "missing", Spec: "auth", Imported: "login"}
	b := analysis.ImportIssue{Reason: "missing", Spec: "auth", Imported: "logout"}
	// distinct Imported => distinct keys.
	if issueKey(a) == issueKey(b) {
		t.Error("issueKey collision for distinct issues")
	}
	// identical fields => identical key.
	if issueKey(a) != issueKey(analysis.ImportIssue{Reason: "missing", Spec: "auth", Imported: "login"}) {
		t.Error("issueKey not stable for identical issues")
	}

	prior := indexIssues([]analysis.ImportIssue{a})
	fresh := filterNewIssues([]analysis.ImportIssue{a, b}, prior)
	if len(fresh) != 1 || issueKey(fresh[0]) != issueKey(b) {
		t.Errorf("filterNewIssues = %v, want just b", fresh)
	}
	if got := filterNewIssues([]analysis.ImportIssue{a}, prior); len(got) != 0 {
		t.Errorf("filterNewIssues(all-prior) = %v, want empty", got)
	}
}
