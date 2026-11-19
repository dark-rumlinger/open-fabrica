package pack

import (
	"strings"
	"testing"
)

// TestNoDecisionInExtended locks the invariant RenderGuidesCore depends on: a
// guide's `## Decision:` block (the architecture choice PLAN must lock) MUST live
// in the CORE, never in a recall-on-demand extended section. If a decision block
// drifted below `<!-- extended -->`, core-only inlining would stop showing it to
// the planner and a locked architecture choice would silently vanish from the
// PLAN prompt. Author decisions above the divider.
func TestNoDecisionInExtended(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	for _, g := range Capabilities(ps) {
		for _, s := range g.Sections {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Title)), "decision") ||
				strings.Contains(strings.ToLower(s.Body), "\n## decision") {
				t.Errorf("guide %q has a Decision block in extended section %q - move it above <!-- extended --> "+
					"so core-only inlining keeps it in the PLAN prompt", g.Slug, s.Anchor)
			}
		}
	}
}

// TestEveryDecisionBlockIsPlanRecordable holds the other half of the invariant.
// stages/plan/STAGE.md tells the planner, twice and in MUST terms, to record a
// `decisions` entry for every `## Decision:` block an inlined guide carries. That
// only works while every such block IS a plan-time architecture choice: a heading
// used for a BUILD-time implementation choice (stream vs whole answer, one-shot vs
// registered provider) hands the planner a MUST it can only satisfy with noise,
// and teaches it that some Decision blocks are optional - which is the reading that
// loses a real one. Those are `## Choose:` blocks; a `## Decision:` says so in its
// heading.
func TestEveryDecisionBlockIsPlanRecordable(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	const marker = "record in the plan's `decisions`"
	for _, g := range Capabilities(ps) {
		for _, line := range strings.Split(g.Body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "## Decision:") {
				continue
			}
			if !strings.Contains(line, marker) {
				t.Errorf("guide %q heading %q must say %q - or, if it is a BUILD-time implementation "+
					"choice rather than an architecture decision PLAN locks, be a `## Choose:` heading instead. "+
					"stages/plan/STAGE.md makes recording every `## Decision:` block mandatory.", g.Slug, line, marker)
			}
		}
	}
}

// TestNoEmptyGuideCores fails the build if any capability guide's CORE trims to
// empty after contract substitution. RenderGuidesCore skips such a guide (an empty
// `<guide>` block is malformed, zero-guidance markup), so a plan that implies the
// guide would build with NO inlined guidance and no error - exactly the silent
// regression the runtime skip guards against. This test is the first line of
// defense: keep every guide's core substantive, above `<!-- extended -->`.
func TestNoEmptyGuideCores(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	for _, g := range Capabilities(ps) {
		if strings.TrimSpace(substituteContracts(g.CoreBody())) == "" {
			t.Errorf("guide %q has an empty CORE after contract substitution - RenderGuidesCore would emit nothing "+
				"for it, so a plan implying this guide builds with no inlined guidance. Add core content above <!-- extended -->.", g.Slug)
		}
	}
}

// perGuideRenderedCap caps ONE guide's rendered core, contracts transcluded.
// end of file
