package engine

// Byte budget for the tool prose that rides EVERY build's cached system prefix.
//
// The stage frames and guide cores each have a ratchet; the always-loaded
// tools' routing block had none, and it is the block most likely to grow
// quietly, assembled from six tools' prose with nobody seeing the total. Going
// over is the signal to cut the prose, not to raise the cap: WhenToUse is for
// disambiguation the model cannot infer; mechanics and facts with a contract or
// guide home belong there instead (tools/AUTHORING.md).

import (
	"testing"

	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// alwaysLoadedProseBudget caps the rendered RoutingProse for the core + backbone set in
// BYTES. Set ~8% above the measured size, so ordinary authoring lands and creeping regrowth
// does not. Lower it in the same change as a real cut - the slack check below prints when
// one has drifted loose, so the ratchet only turns one way.
const alwaysLoadedProseBudget = 7700

// maxProseBudgetSlack is how far under budget the block may sit before the budget itself is
// treated as stale. Printed, not failed: a genuine cut should tighten the cap, but nobody
// should be blocked from landing one.
const maxProseBudgetSlack = 0.15

func alwaysLoadedNames() []string {
	return append(append([]string{}, tools.CoreBuildToolNames...), tools.BuildBackboneToolNames...)
}

func TestAlwaysLoadedRoutingProseBudget(t *testing.T) {
	reg := tools.DefaultRegistry()
	// Full capabilities: the budget must price the widest block any install renders, not
	// whatever a capability-poor test environment happens to gate down to.
	caps := llm.Capabilities{HasLLM: true, HasImage: true, HasStock: true, HasSearch: true,
		HasEmbeddings: true, HasPush: true}
	prose := reg.RoutingProse(alwaysLoadedNames(), caps, StageBuild)
	if len(prose) == 0 {
		t.Fatal("routing prose for the always-loaded set is empty - the registry walk is broken")
	}
	t.Logf("always-loaded routing prose: %d bytes (~%d tokens), budget %d",
		len(prose), len(prose)/4, alwaysLoadedProseBudget)

	switch n := len(prose); {
	case n > alwaysLoadedProseBudget:
		t.Errorf("always-loaded routing prose is %d bytes, over its %d-byte budget by %d.\n"+
			"  This block rides every build's cached prefix. Cut the prose to the disambiguation "+
			"the model cannot infer (tools/AUTHORING.md); raising this cap is the last resort.",
			n, alwaysLoadedProseBudget, n-alwaysLoadedProseBudget)
	case float64(alwaysLoadedProseBudget-n)/float64(alwaysLoadedProseBudget) > maxProseBudgetSlack:
		t.Logf("routing prose is %d bytes against a %d-byte budget (%.0f%% slack) - a cut landed "+
			"without tightening the cap. Lower alwaysLoadedProseBudget to ~%d so the ratchet holds.",
			n, alwaysLoadedProseBudget,
			100*float64(alwaysLoadedProseBudget-n)/float64(alwaysLoadedProseBudget), int(float64(n)*1.08))
	}
}

// TestAlwaysLoadedProseCarriesSchemaNotes is the delivery half: the per-action required
// args and the enum/bound narrowing exist so the model does not have to learn them from a
// runtime rejection, and for the preloaded set THIS is the only path they travel. If they
// stop arriving here they arrive nowhere, because the ground rules forbid find_tool on
// these six tools.
func TestAlwaysLoadedProseCarriesSchemaNotes(t *testing.T) {
	reg := tools.DefaultRegistry()
	prose := reg.RoutingProse(alwaysLoadedNames(), llm.Capabilities{HasLLM: true}, StageBuild)

	// manage_data is the canonical case for both notes: delete requires id while update
	// does not, and search caps limit at 200 where the merged schema advertises query's 500.
	for _, want := range []string{
		"delete(table_name, id)",
		"`manage_data.search`: limit max 200",
	} {
		if !contains(prose, want) {
			t.Errorf("always-loaded routing prose is missing %q.\n"+
				"  The merged union schema cannot express it and find_tool is off-limits for the "+
				"preloaded set, so without this line the model only learns it from a failed call.", want)
		}
	}
}
