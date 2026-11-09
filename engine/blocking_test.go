package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"openfabrica/store"
)

func TestIsBlocking(t *testing.T) {
	cases := []struct {
		name string
		is   Issue
		want bool
	}{
		{"runtime throw blocks", Issue{Category: "runtime", Severity: store.SeverityError, Deterministic: true}, true},
		{"unresolvable specifier blocks", Issue{Category: "module_specifier", Severity: store.SeverityError, Deterministic: true}, true},
		{"ws never welcomed blocks", Issue{Category: "websocket", Severity: store.SeverityError, Deterministic: true}, true},
		{"unparseable module blocks", Issue{Category: "js_syntax", Severity: store.SeverityError, Deterministic: true}, true},

		// The probe already demotes dev-mode console noise to a warning; the gate
		// must honour that split rather than re-deriving it.
		{"runtime warning ships", Issue{Category: "runtime", Severity: store.SeverityWarning, Deterministic: true}, false},
		// A finding that does not reproduce is a flake, and a flake must never be
		// able to stop a deploy.
		{"non-deterministic runtime ships", Issue{Category: "runtime", Severity: store.SeverityError}, false},
		// Call-position inferences: the path may never run. Real findings, but they
		// ship as known issues rather than refusing a deploy.
		{"undefined_reference ships", Issue{Category: "undefined_reference", Severity: store.SeverityError, Deterministic: true}, false},
		{"arity_mismatch ships", Issue{Category: "arity_mismatch", Severity: store.SeverityError, Deterministic: true}, false},
		{"css_syntax ships", Issue{Category: "css_syntax", Severity: store.SeverityError, Deterministic: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBlocking(tc.is); got != tc.want {
				t.Fatalf("isBlocking(%+v) = %v, want %v", tc.is, got, tc.want)
			}
		})
	}
}

// A speculative finding is a structural guess with no runtime witness. It may be
// wrong, so it must never refuse a deploy. store.NormalizeSpeculative is the one
// place that enforces this; this pins the end-to-end guarantee, so a future
// blocking category cannot quietly acquire the power to block on a guess.
func TestSpeculativeCanNeverBlock(t *testing.T) {
	for cat := range blockingCategories {
		in := []Issue{{
			Category:      cat,
			Severity:      store.SeverityError,
			Deterministic: true,
			Speculative:   true,
		}}
		if got := blockingErrors(store.NormalizeSpeculative(in)); len(got) != 0 {
			t.Fatalf("speculative %q finding blocked the deploy: %+v", cat, got)
		}
	}
}

func TestBlockingErrorsSelectsAndOrdersStably(t *testing.T) {
	in := []Issue{
		{Category: "runtime", Severity: store.SeverityError, Deterministic: true, Page: "/z"},
		{Category: "css_syntax", Severity: store.SeverityError, Deterministic: true, Page: "/a"},
		{Category: "runtime", Severity: store.SeverityError, Deterministic: true, Page: "/a"},
	}
	got := blockingErrors(in)
	if len(got) != 2 {
		t.Fatalf("got %d blocking, want 2: %+v", len(got), got)
	}
	if got[0].Page != "/a" || got[1].Page != "/z" {
		t.Fatalf("not ordered stably by fingerprint: %q then %q", got[0].Page, got[1].Page)
	}
}

func TestBlockingCategorySummary(t *testing.T) {
	in := []Issue{
		{Category: "runtime", Severity: store.SeverityError, Deterministic: true, Page: "/a"},
		{Category: "runtime", Severity: store.SeverityError, Deterministic: true, Page: "/b"},
		{Category: "websocket", Severity: store.SeverityError, Deterministic: true},
	}
	if got, want := blockingCategorySummary(blockingErrors(in)), "runtime x2, websocket"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

// The gate is a list of category STRINGS matched against what validators emit, so
// a typo or a renamed category does not fail the build - it silently un-blocks
// that category and the app ships broken again. That is the exact failure this
// gate exists to end, so it must not be able to happen to the gate itself: every
// blocking category has to be one some validator actually raises.
func TestBlockingCategoriesAreActuallyEmitted(t *testing.T) {
	emitted := map[string]bool{}
	for _, dir := range []string{".", "../probe", "validators"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			for cat := range blockingCategories {
				if strings.Contains(string(src), `Category: "`+cat+`"`) ||
					strings.Contains(string(src), `Category:      "`+cat+`"`) ||
					strings.Contains(string(src), `Category: `+cat) {
					emitted[cat] = true
				}
			}
		}
	}
	for cat := range blockingCategories {
		if !emitted[cat] {
			t.Errorf("blocking category %q is emitted by no validator - the gate silently never fires for it", cat)
		}
	}
}
