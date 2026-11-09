package pack

import (
	"strings"
	"testing"
)

// stageFrameBudget caps each stage's assembled frame in BYTES.
//
// This is the non-lexical half of the single-home discipline, and the only half that
// cannot be evaded by rewording. TestRuleSingleHome (engine/) anchors on identifiers,
// so it catches an author who restates a rule reaching for the same vocabulary - but an
// author reaching for a thesaurus slips past it, because no deterministic test decides
// semantic equivalence. A byte budget never reads the words: it prices a restatement
// whatever it is called.
//
// Set ~8% above the measured frame, so ordinary authoring lands and creeping regrowth
// does not. Going over is the signal to find the rule's ONE home and cut the copy
// (knowledge/stages/AUTHORING.md), NOT to raise the cap. When a real cut lands, LOWER
// the budget in the same change - the slack check below prints when one has drifted
// loose, so the ratchet only turns one way.
// expand/plan/build were bumped for the anti-convergence pass (creative anchors,
// world anti-trope list, task floor): new guidance with a single home each, not
// restatements.
//
// plan was bumped again for the deliverables-only rule (no verification/QA/polish
// tasks) and the custom-server-logic tool cue. Both are new guidance with a single
// home, and the frame had 29 bytes of headroom, so there was no cut that would not
// have meant deleting an unrelated rule. The trade also runs the right way: PLAN makes
// a small fraction of BUILD's calls, and the rule removes a costly checklist item.
var stageFrameBudget = map[string]int{
	// Raised for the anti-promotion rule: EXPAND could elevate a tentative aside (a
	// "maybe" in the brief) into a standing subsystem, so the rule fences that off. New guidance with a single home (the "Don't promote an aside" bullet);
	// the addition was tightened to ~100 bytes before raising, not crammed into an existing rule.
	"expand": 9550,
	// Raised for the always-on window-bridge law: the module-scope contract now warns
	// that a `window.<name>` of your own reads undefined and boots dead (the silent twin
	// of the `is not defined` throw). It rides every frame; new guidance, single home.
	// Raised again for the user_flows guide's index line (the guide index rides PLAN):
	// that guide offloads ~4KB from every BUILD turn, so a line here pays for itself.
	// Raised by 100 for the user-flow off qualifier in the guide index line.
	"plan": 53300,
	// Raised for the in-turn ordering rule: calls in one turn run in the order written,
	// so a dependent chain whose arguments are already known is one turn, not one per
	// step. New guidance with a single home (build_loop/one_turn_per_item), and the same
	// change collapsed some duplication first. The trade pays: turn count tracks billed
	// input closely, so batching dependent calls into one turn saves real cost.
	// Raised for the always-on window-bridge law in the module-scope contract (see plan).
	"build": 34800, // +800 for the of_test.js rule
	// live carries the guide recall index, which grows with each new guide.
	"live": 30000, // +300 for keeping of_test.js in step, +200 for the stop rule on a red flow, +100 for the off qualifier
}

// maxBudgetSlack is how far under budget a frame may sit before the budget itself is
// treated as stale. Printed, not failed: a genuine cut should tighten the cap, but
// nobody should be blocked from landing one.
const maxBudgetSlack = 0.15

func TestStageFrameSizeBudget(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	emptySentinels := map[string]string{
		"ALLOWED_COLUMN_TYPES": "", "GUIDES_CATALOGUE": "", "TOOL_CATALOGUE": "",
		"TASK_BUDGET": "", "AMBITION_DIRECTIVE": "", "STACK_NOTE": "",
	}
	for _, st := range []string{"expand", "plan", "build", "live"} {
		budget, ok := stageFrameBudget[st]
		if !ok {
			t.Errorf("stage %q has no frame budget - add one to stageFrameBudget", st)
			continue
		}
		sys := Assemble(AssembleInput{
			Stage: st, Wave: Wave, Sentinels: emptySentinels, Selection: Select(ps, st),
		}).System
		switch n := len(sys); {
		case n > budget:
			t.Errorf("stage %q frame is %d bytes, over its %d-byte budget by %d.\n"+
				"  Prose grew. Find the rule's one home and cut the copy (knowledge/stages/AUTHORING.md) - "+
				"raising this cap is the last resort, not the fix.\n"+
				"  A restatement must cost something even when it is reworded past TestRuleSingleHome's anchors; "+
				"that is what this budget is for.", st, n, budget, n-budget)
		case float64(budget-n)/float64(budget) > maxBudgetSlack:
			t.Logf("stage %q frame is %d bytes against a %d-byte budget (%.0f%% slack) - a cut landed without "+
				"tightening the cap. Lower stageFrameBudget[%q] to ~%d so the ratchet holds.",
				st, n, budget, 100*float64(budget-n)/float64(budget), st, int(float64(n)*1.08))
		}
	}
}

// volatileSentinels are the substitutions whose VALUE changes between two runs on the same
// install: the plan's task budget varies with a project's ambition level, and the stack note
// with whether EXPAND ran. The rest (column types, catalogues) are install-scoped, identical
// for every project, so where they sit costs nothing.
var volatileSentinels = []string{"{{TASK_BUDGET}}", "{{AMBITION_DIRECTIVE}}", "{{STACK_NOTE}}", "{{GUIDES_CATALOGUE}}"}

// TestVolatileSentinelsSitAtTheFrameTail pins the placement rule the whole frame is ordered
// by: anything that varies between runs goes LAST, because a prompt cache serves only the
// prefix that matches, and every byte after a substituted value is re-read at full price by
// the next project. The plan frame carried its task budget a quarter of the way in, which put
// ~38KB of prose that is identical for every project downstream of one variable sentence.
//
// The same rule already governs the seed's date context (engine/seed.go). This is its
// system-prompt half.
func TestVolatileSentinelsSitAtTheFrameTail(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	// A sentinel must not appear before this fraction of the frame.
	//
	// On a large frame every point is worth hundreds of bytes, so the threshold sits just
	// under the closest legitimate sentinel (expand's {{AMBITION_DIRECTIVE}}, ~96%) rather
	// than loose. Positions are logged below, so read the real headroom from a run rather
	// than from this number.
	//
	// The catalogues are emptied above, so a sentinel with a large FILL behind it measures
	// closer to the end here than it sits in the real prompt.
	const minPosition = 0.95

	for _, st := range []string{"expand", "plan", "build", "live"} {
		// Sentinels left UNSUBSTITUTED so their position in the frame is visible.
		sys := Assemble(AssembleInput{
			Stage: st, Wave: Wave, Selection: Select(ps, st),
			Sentinels: map[string]string{"ALLOWED_COLUMN_TYPES": "", "TOOL_CATALOGUE": ""},
		}).System
		for _, s := range volatileSentinels {
			at := strings.Index(sys, s)
			if at < 0 {
				continue
			}
			frac := float64(at) / float64(len(sys))
			t.Logf("stage=%-6s %-24s at %5.1f%% of frame (byte %d of %d)", st, s, frac*100, at, len(sys))
			if frac < minPosition {
				t.Errorf("stage %q places %s at %.0f%% of the frame (byte %d of %d).\n"+
					"  Its value varies per run, so every byte after it is re-read at full price by the\n"+
					"  next project. Move it to the tail and leave a pointer where it was.",
					st, s, frac*100, at, len(sys))
			}
		}
	}
}
