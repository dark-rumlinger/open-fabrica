package engine

import (
	"strings"
	"testing"
)

// PlanScopeAdvisory is advisory-only: a small/normal plan returns "", and a
// large one returns a non-empty flag that names the task count, so an oversized
// single-session build is visible in a post-mortem without ever blocking.
func TestPlanScopeAdvisory(t *testing.T) {
	small := &Plan{Tasks: make([]PlanTaskSpec, 5)}
	if adv := PlanScopeAdvisory(small); adv != "" {
		t.Errorf("a 5-task plan must not be flagged, got %q", adv)
	}

	big := &Plan{Tasks: make([]PlanTaskSpec, largePlanTaskThreshold+2)}
	adv := PlanScopeAdvisory(big)
	if adv == "" {
		t.Fatalf("a %d-task plan must be flagged", largePlanTaskThreshold+2)
	}
	if !strings.Contains(adv, "large plan") {
		t.Errorf("advisory should describe the scope, got %q", adv)
	}

	// Few tasks but many declared files also trips the advisory (context grows
	// with files written, not just task count).
	manyFiles := &Plan{Tasks: []PlanTaskSpec{{Title: "mega"}}}
	for i := 0; i < largePlanTaskThreshold+1; i++ {
		manyFiles.Tasks[0].Paths = append(manyFiles.Tasks[0].Paths, "/files/m"+string(rune('a'+i%26))+string(rune('0'+i/26))+".js")
	}
	if adv := PlanScopeAdvisory(manyFiles); adv == "" {
		t.Errorf("a plan declaring many distinct files must be flagged")
	}

	if adv := PlanScopeAdvisory(nil); adv != "" {
		t.Errorf("nil plan must return no advisory, got %q", adv)
	}
}
