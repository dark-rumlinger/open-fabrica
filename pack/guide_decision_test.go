package pack

import (
	"strings"
	"testing"
)

// The PLAN stage carries no tools and emits a JSON plan, so it can neither write
// code nor recall a section. RenderGuidesDecision is the facet that follows from
// that: the choices a planner locks, and nothing addressed to a stage it is not.
// These guards pin both halves - every choice block survives, and the build-only
// mechanics do not.

// choiceHeadings returns every `## Decision:` / `## Choose:` heading in a guide's
// core, read straight from the tree rather than from the renderer.
func choiceHeadings(core string) []string {
	var out []string
	for _, line := range strings.Split(core, "\n") {
		for _, h := range decisionHeadings {
			if strings.HasPrefix(line, h) {
				out = append(out, strings.TrimSpace(line))
			}
		}
	}
	return out
}

// TestPlanFacetCarriesEveryChoiceBlock: a choice the planner must record cannot be
// dropped by the facet. This is the half that would fail silently - a planner that
// never sees a `## Decision:` block simply omits the decision, and the build then
// picks the model for itself.
func TestPlanFacetCarriesEveryChoiceBlock(t *testing.T) {
	guides := Capabilities(embedded(t))
	seen := 0
	for _, g := range guides {
		heads := choiceHeadings(g.CoreBody())
		if len(heads) == 0 {
			continue
		}
		facet := RenderGuidesDecision([]Pack{g})
		for _, h := range heads {
			seen++
			if !strings.Contains(facet, h) {
				t.Errorf("guide %q: plan facet drops choice block %q", g.Slug, h)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no choice blocks found in the tree - this guard would pass vacuously")
	}
	t.Logf("choice blocks carried into the plan facet: %d", seen)
}

// TestPlanFacetOmitsBuildMechanics: the facet must not carry a core's build-time
// body. Anchored on real headings from the two largest cores, so a regression that
// quietly reverts the facet to a full-core render fails here.
func TestPlanFacetOmitsBuildMechanics(t *testing.T) {
	facet := RenderGuidesDecision(Capabilities(embedded(t)))
	// Every canary must be a heading that is still in a CORE. The facet lists EXTENDED
	// section titles on purpose (the "what else this guide covers" note), so a heading
	// that moves below <!-- extended --> stops being evidence of a full-core render and
	// starts false-firing - which is what "Bind to the DOM the page DECLARES" did once it
	// was moved to recall. Re-point a canary when you move its section, never delete one.
	for _, mech := range []string{
		"The null-canvas crash",
		"Don't call through a live binding before boot sets it",
		"The import tree: entry is the unimported root",
		"System frames the server emits",
	} {
		if strings.Contains(facet, mech) {
			t.Errorf("plan facet carries build-time mechanics %q - PLAN cannot act on it "+
				"(no tools, JSON output); BUILD gets it via RenderGuidesCore", mech)
		}
	}
}
