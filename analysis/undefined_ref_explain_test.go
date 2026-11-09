package analysis

import (
	"strings"
	"testing"
)

// A name that resolves nowhere but sits one slip away from a real one is steered
// at that name; an unrelated name keeps the generic fallback.
func TestExplainUndefinedRefNearName(t *testing.T) {
	exportIdx := map[string][]string{"getCount": {"js/state.js"}}
	declIdx := map[string][]string{
		"getCount":  {"js/state.js"},
		"renderRow": {"js/view.js"},
	}
	byName := map[string]string{"state": "js/state.js", "view": "js/view.js"}

	// Affix slip of an exported name: steered to the exporter with the import line.
	got := ExplainUndefinedRef("js/view.js", UndefinedRef{Name: "count"}, exportIdx, declIdx, byName)
	for _, want := range []string{"`getCount`", "state.js", "import { getCount } from 'state';"} {
		if !strings.Contains(got, want) {
			t.Errorf("near-name steer missing %q, got: %s", want, got)
		}
	}

	// One-character typo of a name declared (not exported) in a sibling.
	got = ExplainUndefinedRef("js/state.js", UndefinedRef{Name: "renderRw"}, exportIdx, declIdx, byName)
	if !strings.Contains(got, "`renderRow`") || !strings.Contains(got, "view.js") {
		t.Errorf("typo steer should name the near name and its module, got: %s", got)
	}

	// Nothing close: the generic fallback stands.
	got = ExplainUndefinedRef("js/view.js", UndefinedRef{Name: "zzUnrelated"}, exportIdx, declIdx, byName)
	if !strings.Contains(got, "define it in this module") {
		t.Errorf("an unrelated name must keep the generic steer, got: %s", got)
	}

	// A near name that exactly resolves is handled by the earlier branches, so the
	// near-name text never appears for a name a module actually exports.
	got = ExplainUndefinedRef("js/view.js", UndefinedRef{Name: "getCount"}, exportIdx, declIdx, byName)
	if strings.Contains(got, "closest existing name") {
		t.Errorf("a resolvable name must not take the near-name path, got: %s", got)
	}
}

// A DECLARED module library matches its conventional binding case-insensitively
// (THREE -> three), so the very first file to touch the library gets the exact
// import line even before any sibling imports it. Assignments, classic libs, and
// unrelated names stay out.
func TestExplainDeclaredExternalRef(t *testing.T) {
	libs := []ExternalLib{
		{Name: "three", Kind: "module", ModuleName: "three", URL: "https://cdn/x.js"},
		{Name: "marked", Kind: "classic", GlobalName: "marked", URL: "https://cdn/m.js"},
	}
	got := ExplainDeclaredExternalRef(UndefinedRef{Name: "THREE", Kind: RefMember}, libs)
	for _, want := range []string{"declared `three` library", "import * as THREE from 'three';"} {
		if !strings.Contains(got, want) {
			t.Errorf("declared-library steer missing %q, got: %s", want, got)
		}
	}
	if got := ExplainDeclaredExternalRef(UndefinedRef{Name: "THREE", Kind: RefAssign}, libs); got != "" {
		t.Errorf("assigning to a library binding is a different bug; want no steer, got: %s", got)
	}
	if got := ExplainDeclaredExternalRef(UndefinedRef{Name: "marked", Kind: RefMember}, libs); got != "" {
		t.Errorf("a classic library supplies a global, not an import; want no steer, got: %s", got)
	}
	if got := ExplainDeclaredExternalRef(UndefinedRef{Name: "zzUnrelated", Kind: RefMember}, libs); got != "" {
		t.Errorf("an unrelated name must not match any declared library, got: %s", got)
	}
}

func TestNearNameRank(t *testing.T) {
	cases := []struct {
		miss, cand string
		want       int
	}{
		{"getcount", "getCount", 1}, // case slip
		{"count", "getCount", 2},    // affix
		{"cammera", "camera", 3},    // one-edit typo
		{"pos", "getPos", 0},        // affix too short to trust
		{"row", "rw", 0},            // short names never fuzzy-match
		{"alpha", "omega", 0},       // unrelated
	}
	for _, c := range cases {
		if got := nearNameRank(c.miss, c.cand); got != c.want {
			t.Errorf("nearNameRank(%q, %q) = %d, want %d", c.miss, c.cand, got, c.want)
		}
	}
}

func TestWithinOneEdit(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"abcde", "abcde", true},  // equal
		{"abcde", "abXde", true},  // substitution
		{"abcde", "abcdef", true}, // insertion
		{"abcdef", "abcde", true}, // deletion
		{"abcde", "abXYe", false}, // two edits
		{"abc", "abcde", false},   // length gap over one
	}
	for _, c := range cases {
		if got := withinOneEdit(c.a, c.b); got != c.want {
			t.Errorf("withinOneEdit(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
