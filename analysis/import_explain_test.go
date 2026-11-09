package analysis

import (
	"strings"
	"testing"
)

// Locks the canonical unresolved-import wording: the post-build validator
// and the check action both render through RenderImportProblems. If a future edit
// forks one surface back to a private renderer, its output diverges from this
// pinned text and the surfaces drift again - this test is the tripwire.

func TestExplainImportIssue_NameMissing(t *testing.T) {
	got := explainImportIssue(ImportIssue{
		Imported: "state", Spec: "./x.js", Reason: "name_missing",
		Exports: []string{"init", "reset"}, HasDefault: true,
	})
	for _, want := range []string{"imports `state` from `./x.js`", "declares no such export", "it exports: init, reset", "(plus a default export)"} {
		if !strings.Contains(got, want) {
			t.Errorf("name_missing line missing %q; got: %s", want, got)
		}
	}
}

func TestExplainImportIssue_TargetAbsent(t *testing.T) {
	got := explainImportIssue(ImportIssue{Imported: "draw", Spec: "./render.js", Reason: "target_absent"})
	for _, want := range []string{"imports `draw` from `./render.js`", "no module resolves that specifier"} {
		if !strings.Contains(got, want) {
			t.Errorf("target_absent line missing %q; got: %s", want, got)
		}
	}
}

func TestRenderImportProblems_ListsAllAndClosesOnce(t *testing.T) {
	msg := RenderImportProblems("js/main.js", []ImportIssue{
		{Imported: "a", Spec: "./x.js", Reason: "name_missing", Exports: []string{"b"}},
		{Imported: "c", Spec: "./y.js", Reason: "target_absent"},
	})
	if !strings.HasPrefix(msg, "module `js/main.js` has unresolved imports") {
		t.Errorf("header drifted: %s", msg)
	}
	if strings.Count(msg, "\n  - ") != 2 {
		t.Errorf("want 2 issue lines, got: %s", msg)
	}
	// One shared footer that covers both a wrong name and a missing target.
	if !strings.Contains(msg, "Fix each import name") {
		t.Errorf("footer drifted: %s", msg)
	}
}

// TestFirstOtherModule pins the "name the module that actually owns this symbol"
// steer that every undefined-reference explainer routes through.
func TestFirstOtherModule(t *testing.T) {
	srcs := []string{"a.js", "b.js", "self.js", "c.js"}
	if got := firstOtherModule(srcs, "a.js"); got != "b.js" {
		t.Errorf("firstOtherModule = %q, want b.js", got)
	}
	if got := firstOtherModule([]string{"self.js"}, "self.js"); got != "" {
		t.Errorf("firstOtherModule(only self) = %q, want empty", got)
	}
	if got := firstOtherModule(nil, "self.js"); got != "" {
		t.Errorf("firstOtherModule(nil) = %q, want empty", got)
	}
}
