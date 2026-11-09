package engine

import (
	"strings"
	"testing"

	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// Assembled-prompt ratchets for PLAN and LIVE. The per-stage frame budgets in
// pack/promptsize_test.go hold the catalogues out, and the BUILD floor gate in
// build_prompt_size_test.go covers BUILD only, so these two assembled prompts
// had no gate at all and could grow without tripping anything.

// planAssembledWorstCase renders the PLAN system prompt at its largest real
// shape: the full build tool catalogue plus EVERY guide's decision facet (the
// unrouted fallback used when the guide pre-pass cannot run).
func planAssembledWorstCase(t *testing.T) string {
	t.Helper()
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	reg := tools.DefaultRegistry()
	return pack.Assemble(planAssembleInput(ps, reg, llm.Capabilities{}, map[string]string{
		"ALLOWED_COLUMN_TYPES": "`" + strings.Join(allowedColumnTypes, "` | `") + "`",
		"GUIDES_CATALOGUE":     pack.RenderGuidesDecision(pack.Capabilities(ps)),
		"TOOL_CATALOGUE":       reg.Index(llm.Capabilities{}, StageBuild),
		"TASK_BUDGET":          "",
		"STACK_NOTE":           "",
	})).System
}

// planAssembledBudget caps the fully-assembled PLAN system prompt. It is re-sent
// on every PLAN call (up to four per run counting corrective retries), so growth
// here is multiplied. Raising the cap is the last resort, not the fix.
// Bumped for the anti-convergence pass (task floor directive, leaf scaling),
// then for the phaser_2d and physics_3d guides and the engine pin facets
// (the pinned library URLs must reach PLAN, where external_libraries is written).
const planAssembledBudget = 76_300

func TestPlanAssembledPromptBudget(t *testing.T) {
	sys := planAssembledWorstCase(t)
	t.Logf("PLAN assembled system prompt: %d bytes (~%d tokens)", len(sys), len(sys)/4)
	if len(sys) > planAssembledBudget {
		t.Errorf("PLAN assembled system prompt is %d bytes (~%d tokens), over its %d-byte budget by %d.\n"+
			"  Every PLAN call re-sends this. Find what grew (frame, tool catalogue, or guide decision facets)\n"+
			"  and cut it; raising the cap is the last resort, not the fix.",
			len(sys), len(sys)/4, planAssembledBudget, len(sys)-planAssembledBudget)
	}
}

// liveAssembledBudget caps the fully-assembled LIVE system prompt (frame +
// deferred-tool index + routing prose), re-read on every live wake turn.
// Raised for of_test.js user-flow testing (manage_testing write_flows/test_flows and the
// build rule that every interactive project ships one): new capability, single home.
// Raised by 200 for the live stop rule: a flow still red after 3 replays without progress
// is reported to the owner instead of chased.
// Raised by 100 for the live rule's user-flow off qualifier.
const liveAssembledBudget = 43_000

func TestLiveAssembledPromptBudget(t *testing.T) {
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	sys := w.liveSystemPrompt()
	t.Logf("LIVE assembled system prompt: %d bytes (~%d tokens)", len(sys), len(sys)/4)
	if len(sys) > liveAssembledBudget {
		t.Errorf("LIVE assembled system prompt is %d bytes (~%d tokens), over its %d-byte budget by %d.\n"+
			"  Every live turn re-reads this. Find what grew (frame, tool index, or routing prose)\n"+
			"  and cut it; raising the cap is the last resort, not the fix.",
			len(sys), len(sys)/4, liveAssembledBudget, len(sys)-liveAssembledBudget)
	}
}
