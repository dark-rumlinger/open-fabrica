package engine

// Core-tool claim drift guard.
//
// knowledge/mental-model.md tells the PLAN model which tools arrive preloaded, and it
// rides every PLAN prompt. The preloaded tiers must be claimed in ONE place: the schema
// description names the field and defers rather than respelling them, so only the
// always-on home is checked here.
//
// The truth is the registry: tools.CoreBuildToolNames always, and
// tools.BuildBackboneToolNames when planNeedsBackbone says the plan does data work. The
// single-home guards check that a rule lives in ONE place, not that the place names the
// ACTUAL preloaded set, so derive both tiers from the registry and assert every surface
// spells each of them. A tool added to, removed from, or renamed in either tier fails
// here until every prompt surface is updated with it.

import (
	"strings"
	"testing"

	"openfabrica/knowledge"
	"openfabrica/pack"
	"openfabrica/tools"
)

// toolShorthand renders a tier the way the prompt prose spells it: the tool name
// minus its manage_ prefix, de-pluralized (manage_files -> file, manage_tasks ->
// task, manage_testing -> testing). Derived rather than written down, so the
// expectation moves with the registry.
func toolShorthand(names []string) string {
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, strings.TrimSuffix(strings.TrimPrefix(n, "manage_"), "s"))
	}
	return strings.Join(parts, "/")
}

func TestCoreToolClaimMatchesRegistry(t *testing.T) {
	core := toolShorthand(tools.CoreBuildToolNames)
	backbone := toolShorthand(tools.BuildBackboneToolNames)
	// Guard the guard: a shrunken registry would make the assertions below pass
	// against almost any prose.
	if n := strings.Count(core, "/") + 1; n < 3 {
		t.Fatalf("core tier has only %d tools (%q) - did the registry move?", n, core)
	}
	if n := strings.Count(backbone, "/") + 1; n < 3 {
		t.Fatalf("backbone tier has only %d tools (%q) - did the registry move?", n, backbone)
	}

	mm, err := knowledge.MentalModel()
	if err != nil {
		t.Fatalf("read mental model: %v", err)
	}

	for surface, body := range map[string]string{
		"knowledge/mental-model.md": mm,
	} {
		for tier, want := range map[string]string{"core": core, "backbone": backbone} {
			if strings.Contains(body, want) {
				continue
			}
			t.Errorf("%s does not name the %s tool tier as %q.\n"+
				"Every prompt surface that claims which tools are preloaded must agree with "+
				"the registry: tools.CoreBuildToolNames always, tools.BuildBackboneToolNames "+
				"when planNeedsBackbone fires. A surface that undersells a tier makes the "+
				"planner waste a find_tool turn re-loading something it already holds; one "+
				"that oversells it makes the planner skip a tool it needs.", surface, tier, want)
		}
	}
}

// TestGroundRulesBackboneClaimIsConditional pins the OTHER always-on surface. The
// _common_rules fragment rides every BUILD and LIVE prompt; it once claimed the
// schema/api/data backbone was unconditionally pre-loaded in BUILD, while
// planNeedsBackbone gates it - so the model called an unloaded tool, was refused,
// and paid a cache-busting mid-session schema append.
func TestGroundRulesBackboneClaimIsConditional(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	var body string
	for _, p := range ps {
		if p.Slug == "_common_rules" {
			body = p.Body
		}
	}
	if body == "" {
		t.Fatal("_common_rules fragment not found")
	}
	if !strings.Contains(body, toolShorthand(tools.BuildBackboneToolNames)) {
		t.Fatalf("_common_rules no longer names the backbone tier as %q", toolShorthand(tools.BuildBackboneToolNames))
	}
	// The claim must carry its condition: the tier is preloaded only when the
	// plan does data work (and always in LIVE).
	if !strings.Contains(body, "table or CRUD work") {
		t.Error("_common_rules states the backbone preload without its condition - " +
			"planNeedsBackbone gates the tier, so an unconditional claim makes the model " +
			"call manage_schema when it is not loaded")
	}
}
