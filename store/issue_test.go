package store

import "testing"

// NormalizeSpeculative is the single enforcement point for the invariant that a
// structural inference with no runtime witness never drives a destructive
// fix-pass: a speculative finding is forced to a non-looping warning whatever the
// validator set, and a non-speculative finding is untouched.
func TestNormalizeSpeculative(t *testing.T) {
	in := []Issue{
		// A speculative error must be downgraded.
		{Severity: SeverityError, Category: "orphaned_module", File: "a.js", Deterministic: true, Speculative: true},
		// A genuine deterministic error is left alone.
		{Severity: SeverityError, Category: "js_syntax", File: "b.js", Deterministic: true},
		// A non-speculative warning is left alone.
		{Severity: SeverityWarning, Category: "perf", File: "c.js"},
	}
	out := NormalizeSpeculative(in)

	if out[0].Severity != SeverityWarning || out[0].Deterministic {
		t.Errorf("speculative finding not downgraded: %+v", out[0])
	}
	if out[1].Severity != SeverityError || !out[1].Deterministic {
		t.Errorf("genuine error must be untouched: %+v", out[1])
	}
	if out[2].Severity != SeverityWarning {
		t.Errorf("non-speculative warning must be untouched: %+v", out[2])
	}
}
