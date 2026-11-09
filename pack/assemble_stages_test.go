package pack

import (
	"strings"
	"testing"
)

// TestStageAssemblyMarkers locks down the per-stage system-prompt assembly: each
// stage frame, the build/live-only common-rules fragment, the live tool-catalogue
// intro, and sentinel/contract substitution must all survive. It is the net for
// the engine's four stage assemblers (which feed Assemble the same Selection +
// inputs).
func TestStageAssemblyMarkers(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}

	// commonRulesMarker is a distinctive phrase from the _common_rules fragment.
	const commonRulesMarker = "The core tools are already loaded"
	// toolIntroMarker is the find_tool guidance that must precede a tool catalogue.
	const toolIntroMarker = "Scan the whole catalogue first"

	// The always-on platform layer rides every stage's system prompt.
	const mentalModelMarker = "How this platform works"
	const guideIndexMarker = "## Guide index"

	t.Run("build", func(t *testing.T) {
		sys := Assemble(AssembleInput{Stage: "build", Wave: "w", Selection: Select(ps, "build")}).System
		mustContain(t, sys, "<!-- w -->", mentalModelMarker, guideIndexMarker, "Build the project", "Client JS module split", "Subsystem-split build order", commonRulesMarker)
		// build passes no ToolIndex - no catalogue, no intro.
		mustNotContain(t, sys, toolIntroMarker)
	})

	t.Run("live", func(t *testing.T) {
		sys := Assemble(AssembleInput{Stage: "live", Wave: "w", ToolIndex: "TOOL_INDEX_BODY", Selection: Select(ps, "live")}).System
		mustContain(t, sys, "Respond to owner", commonRulesMarker, toolIntroMarker, "TOOL_INDEX_BODY")
	})

	t.Run("plan", func(t *testing.T) {
		sentinels := map[string]string{
			"ALLOWED_COLUMN_TYPES": "text|int",
			"GUIDES_CATALOGUE":     "FULL_GUIDES_BODY",
			"TOOL_CATALOGUE":       "tools",
			"TASK_BUDGET":          "budget",
			"STACK_NOTE":           "stack",
		}
		sys := Assemble(AssembleInput{Stage: "plan", Wave: "w", Sentinels: sentinels, Selection: Select(ps, "plan")}).System
		// The plan frame inlines the full guides (build/live do not).
		mustContain(t, sys, "Produce the project plan", mentalModelMarker, guideIndexMarker, "FULL_GUIDES_BODY")
		// Every provided sentinel must be substituted (no leftover token).
		for k := range sentinels {
			if strings.Contains(sys, "{{"+k+"}}") {
				t.Errorf("plan: sentinel {{%s}} not substituted", k)
			}
		}
		// common rules are build/live-only - never in the plan frame.
		mustNotContain(t, sys, commonRulesMarker)
	})

	t.Run("expand", func(t *testing.T) {
		sys := Assemble(AssembleInput{Stage: "expand", Wave: "w", Sentinels: map[string]string{"AMBITION_DIRECTIVE": "DIRECTIVE_X"}, Selection: Select(ps, "expand")}).System
		// EXPAND keeps only the TOP of the mental model (what the platform is): the
		// artifact kinds, helper modules, wire-shape Platform laws, and guide index
		// are all cut - a behaviour-level rewrite must not carry the transport
		// nouns its own frame bans.
		mustContain(t, sys, "Rewrite a sparse owner brief", "DIRECTIVE_X", mentalModelMarker)
		mustNotContain(t, sys, "{{AMBITION_DIRECTIVE}}", commonRulesMarker, guideIndexMarker, "## Platform laws",
			"## The artifacts you create", "create_websocket")
	})
}

func mustContain(t *testing.T, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("assembled prompt missing %q", w)
		}
	}
}

func mustNotContain(t *testing.T, s string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(s, u) {
			t.Errorf("assembled prompt unexpectedly contains %q", u)
		}
	}
}
