package engine

import (
	"encoding/json"
	"testing"
)

// planSchemaDescriptionBudget caps the DESCRIPTION prose in planSchemaJSON, in bytes.
//
// This schema is pinned onto the same CompletionRequest as the assembled PLAN frame
// (stage_plan.go), and re-sent on every coherence retry. Every byte of judgment written
// into a field description is therefore the frame's rule a second time, in different
// words, inside one prompt - the exact failure knowledge/stages/AUTHORING.md exists to
// kill, and the one prompt surface no budget priced while it grew to 5,681 bytes.
//
// The other ratchets could not see it: pack/promptsize_test.go measures the assembled
// frame only, and engine/build_prompt_size_test.go is the BUILD floor. Descriptions NAME
// each field; the frame decides it. Over budget means a judgment crept back in - move it
// to the frame rather than raising this cap.
const planSchemaDescriptionBudget = 2350

// planSchemaDescriptionBytes walks the decoded schema and sums every "description"
// string, at any depth. Derived from the shipped JSON, so it cannot drift from what the
// provider actually receives.
func planSchemaDescriptionBytes(t *testing.T) int {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(planSchemaJSON), &doc); err != nil {
		t.Fatalf("planSchemaJSON does not parse: %v", err)
	}
	total := 0
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				if s, ok := child.(string); ok && k == "description" {
					total += len(s)
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(doc)
	return total
}

func TestPlanSchemaDescriptionBudget(t *testing.T) {
	got := planSchemaDescriptionBytes(t)
	t.Logf("planSchemaJSON descriptions: %d bytes (~%d tokens), budget %d",
		got, got/4, planSchemaDescriptionBudget)
	if got > planSchemaDescriptionBudget {
		t.Errorf("planSchemaJSON descriptions are %d bytes, over the %d-byte budget by %d.\n"+
			"  These ride the SAME request as the PLAN frame, which already carries the judgment.\n"+
			"  Name the field here and state the rule once in knowledge/stages/plan/STAGE.md;\n"+
			"  raising this cap is the last resort, not the fix.", got, planSchemaDescriptionBudget, got-planSchemaDescriptionBudget)
	}
	// The ratchet turns one way: a real cut should tighten the cap rather than bank slack.
	if slack := float64(planSchemaDescriptionBudget-got) / float64(planSchemaDescriptionBudget); slack > 0.15 {
		t.Logf("budget has drifted %.0f%% loose - lower planSchemaDescriptionBudget to ~%d",
			slack*100, int(float64(got)*1.08))
	}
}
