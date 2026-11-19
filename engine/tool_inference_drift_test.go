package engine

// Drift guard for the task-tool inference table. taskToolInference maps
// task-text keywords to registered tool names by hand; a renamed or removed
// tool would leave the inference silently recommending a name the registry no
// longer serves (the schema preload skips unknown names). This pins every
// referenced tool to the real registry.

import (
	"slices"
	"testing"

	"openfabrica/pack"
	"openfabrica/tools"
	_ "openfabrica/tools/all" // register every tool category via init()
)

func TestTaskToolInferenceReferencesRealTools(t *testing.T) {
	reg := tools.DefaultRegistry()
	for _, inf := range taskToolInference {
		if !reg.Has(inf.tool) {
			t.Errorf("taskToolInference references %q, which is not a registered tool - update the table (engine/build_tools.go) to the tool's current name or remove the entry", inf.tool)
		}
		if len(inf.keywords) == 0 {
			t.Errorf("taskToolInference entry %q has no keywords and can never fire - remove it", inf.tool)
		}
	}
}

// TestTaskInferenceToolsRouteToAGuide pins the OTHER half of the table: a
// keyword firing an inferred tool is only useful if that tool then pulls its
// guide. What can rot is an entry whose tool no guide claims, which preloads a
// schema and no guidance. A guideless tool is legitimate and listed explicitly,
// so ADDING one is a deliberate edit here rather than a silent gap.
func TestTaskInferenceToolsRouteToAGuide(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	// Tools that deliberately own no guide: their capability is small enough that
	// the tool's own schema and description are the whole contract.
	guideless := map[string]bool{
		"manage_email":      true,
		"manage_schedules":  true,
		"manage_web_search": true,
		"manage_web_fetch":  true,
	}
	toolGuide := guideToolMap(ps) // tool name -> owning guide slug (derived from guide `tools:`)
	for _, inf := range taskToolInference {
		_, owned := toolGuide[inf.tool]
		if owned && guideless[inf.tool] {
			t.Errorf("tool %q is listed as guideless but a guide now claims it - drop it from the exemption list", inf.tool)
			continue
		}
		if !owned && !guideless[inf.tool] {
			t.Errorf("taskToolInference infers %q, which no guide's `tools:` claims - inferring it preloads a schema with no contract behind it; add the tool to its guide or list it as guideless here", inf.tool)
		}
	}
}

// An agent task declared manage_agent (and manage_llm_proxy) but not
// manage_function, which the agent's own tools are built with, so the build loaded it
// with find_tool mid-segment and re-bought the cached prefix. The agent tool implies it.
func TestBuildPreloadTools_AgentImpliesFunction(t *testing.T) {
	names := buildPreloadTools(Task{Title: "Assistant agent", Tools: []string{"manage_agent", "manage_llm_proxy"}}, false)
	if !slices.Contains(names, "manage_function") {
		t.Fatalf("an agent task must preload manage_function, got %v", names)
	}
}
