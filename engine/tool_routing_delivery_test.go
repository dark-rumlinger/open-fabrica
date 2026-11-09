package engine

// Delivery guard for tool routing prose.
//
// WhenToUse is mandatory and guarded, but the provider wire schema carries only
// the OneLiner, and WhenToUse is emitted by exactly one code path - the
// find_tool handler - which the preloaded core set never goes through. These
// assert the two halves that deliver it anyway: the always-loaded set on the
// byte-stable system prefix, and the plan-derived extras in the build's stable
// intent block.

import (
	"strings"
	"testing"

	"openfabrica/providers/llm"
	"openfabrica/tools"

	// Blank-import the tool subpackages so the registry is populated.
	_ "openfabrica/tools/all" // register every tool category via init()
)

// TestRoutingProseRendersEveryRequestedTool is the unit the two delivery paths
// depend on: every named, available tool contributes its name and its full
// WhenToUse, and the output is deterministic.
func TestRoutingProseRendersEveryRequestedTool(t *testing.T) {
	reg := tools.DefaultRegistry()
	names := append(append([]string{}, tools.CoreBuildToolNames...), tools.BuildBackboneToolNames...)
	var caps llm.Capabilities
	out := reg.RoutingProse(names, caps, StageBuild)
	if out == "" {
		t.Fatal("RoutingProse rendered nothing for the always-loaded set")
	}
	for _, n := range names {
		if !strings.Contains(out, "`"+n+"`") {
			t.Errorf("RoutingProse dropped %q", n)
		}
	}
	for _, td := range reg.List() {
		if !containsName(names, td.Name) {
			continue
		}
		when := strings.TrimSpace(td.WhenToUse)
		if when != "" && !strings.Contains(out, when) {
			t.Errorf("%q appears without its full WhenToUse - the routing guidance is the point of the block", td.Name)
		}
	}
	if out != reg.RoutingProse(names, caps, StageBuild) {
		t.Error("RoutingProse must be deterministic - it rides a cache-stable prompt prefix")
	}
	// An unknown or gated name is dropped, never rendered as a phantom tool.
	if strings.Contains(reg.RoutingProse([]string{"manage_nonexistent"}, caps, StageBuild), "manage_nonexistent") {
		t.Error("RoutingProse rendered a tool that is not registered")
	}
}

// The always-loaded half. A build session preloads these and the ground rules
// forbid find_tool-ing them, so the system prompt is the only place their routing
// prose can arrive.
func TestBuildSystemPromptCarriesAlwaysLoadedRoutingProse(t *testing.T) {
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	sys := w.buildSystemPrompt(&Plan{})
	for _, n := range append(append([]string{}, tools.CoreBuildToolNames...), tools.BuildBackboneToolNames...) {
		when := strings.TrimSpace(mustTool(t, n).WhenToUse)
		if when != "" && !strings.Contains(sys, when) {
			t.Errorf("the BUILD system prompt does not carry %q's WhenToUse. It is preloaded, so find_tool "+
				"(the only other path that emits it) never runs for it - the guidance would reach nobody.", n)
		}
	}
}

// The plan-derived half. These are preloaded from the plan's `tools` declarations,
// so the same reasoning applies, and the disambiguation they carry ("pick a
// DIFFERENT realtime tool for other models") is exactly what decides whether a
// multiplayer build is a relay, a tick, or an authoritative reducer. It rides the SHARED
// block rather than the project-intent block: the prose is the same for any plan that
// pulls the same tools, so it belongs ahead of text unique to this project.
func TestBuildSharedSeedCarriesPlanToolRoutingProse(t *testing.T) {
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	plan := &Plan{
		Goal: "a multiplayer game",
		Tasks: []PlanTaskSpec{
			{ID: "net", Title: "Realtime lobby", Intent: "players share a room", Tools: []string{"manage_relay"}},
		},
	}
	shared := w.buildSharedSeed(plan)
	intent := w.buildIntentSeed(plan)

	if !strings.Contains(shared, strings.TrimSpace(mustTool(t, "manage_relay").WhenToUse)) {
		t.Error("the shared block does not carry manage_relay's WhenToUse, so the plan preloaded the " +
			"tool and withheld the one paragraph that says when to use it over manage_tick / manage_authoritative")
	}
	// The always-loaded set has its own home on the byte-stable system prefix;
	// repeating it per project would pay for it twice.
	if strings.Contains(shared, strings.TrimSpace(mustTool(t, "manage_files").WhenToUse)) {
		t.Error("the shared block repeats an always-loaded tool's WhenToUse - that set belongs on the system prefix")
	}
	// The split is the point: project-unique text must not sit inside the shared block, or
	// none of the shared bytes can be reused by another project or another build.
	if strings.Contains(shared, "a multiplayer game") {
		t.Error("the shared block carries this project's goal; project-unique text belongs in the intent block")
	}
	if !strings.Contains(intent, "a multiplayer game") {
		t.Error("the intent block dropped the project goal")
	}
	if strings.Contains(intent, strings.TrimSpace(mustTool(t, "manage_relay").WhenToUse)) {
		t.Error("the intent block repeats the shared routing prose")
	}
}

func mustTool(t *testing.T, name string) *tools.ToolDef {
	t.Helper()
	for _, td := range tools.DefaultRegistry().List() {
		if td.Name == name {
			return td
		}
	}
	t.Fatalf("%q is not registered", name)
	return nil
}

func containsName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
