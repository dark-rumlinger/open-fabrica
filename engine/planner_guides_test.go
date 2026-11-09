package engine

// Wiring guard for planner-declared guides.
//
// The guides below have no tool or library of their own, so the PLAN frame
// names each one in prose and tells the planner when to declare it. This test
// pins both halves: the slug is real (else planGuideSlugs silently drops it)
// and the PLAN frame names it (else the planner is never told). A renamed guide
// or a dropped frame instruction is caught in CI.

import (
	"strings"
	"testing"

	"openfabrica/pack"
)

// plannerDeclaredGuides are the slugs the PLAN frame names in prose because no
// tool or library signal reaches them. The terrain pair also rides a locked
// decision, but the frame instruction is what tells the planner to lock it.
var plannerDeclaredGuides = []string{
	"webrtc_signaling",
	"e2ee",
	"voxel_worlds",
	"heightfield_terrain",
	"game_feel",
}

func TestPlannerDeclaredGuidesAreRealAndWired(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	valid := validCapabilitySlugs(ps)

	var planBody string
	for _, p := range ps {
		if p.Role == pack.RoleStage && p.Stage == "plan" {
			planBody = p.Body
			break
		}
	}
	if planBody == "" {
		t.Fatal("plan stage pack not found")
	}

	for _, slug := range plannerDeclaredGuides {
		if !valid[slug] {
			t.Errorf("planner-declared guide %q is not a real capability slug - plan.Guides would silently drop it", slug)
		}
		if !strings.Contains(planBody, slug) {
			t.Errorf("the PLAN frame does not name guide %q, so the planner is never told to declare it and it never reaches BUILD", slug)
		}
	}
}

// matchedGuideSlugs is the actual prefetch selection. This exercises both
// delivery paths end to end: a planner-declared slug survives, a tool-inferred
// guide is auto-selected, and a bogus slug is dropped.
func TestMatchedGuideSlugs_DeliversDeclaredAndInferred(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	// Planner-declared: a payments brief lists the slug explicitly.
	pay := &Plan{Guides: []string{"payments"}, Tasks: []PlanTaskSpec{{Title: "Checkout", Tools: []string{"manage_payments"}}}}
	if !containsStr(matchedGuideSlugs(pay, ps), "payments") {
		t.Errorf("payments plan declaring guides:[payments] must deliver the payments guide, got %v", matchedGuideSlugs(pay, ps))
	}

	// AI brief declaring llm_endpoints.
	ai := &Plan{Guides: []string{"llm_endpoints"}}
	if !containsStr(matchedGuideSlugs(ai, ps), "llm_endpoints") {
		t.Errorf("AI plan declaring guides:[llm_endpoints] must deliver the llm_endpoints guide, got %v", matchedGuideSlugs(ai, ps))
	}

	// Tool-inferred: a realtime task auto-prefetches realtime_signaling with no plan.Guides entry.
	rt := &Plan{Tasks: []PlanTaskSpec{{Title: "Chat", Tools: []string{"manage_relay"}}}}
	if !containsStr(matchedGuideSlugs(rt, ps), "realtime_signaling") {
		t.Errorf("a realtime task must auto-prefetch realtime_signaling, got %v", matchedGuideSlugs(rt, ps))
	}

	// A bogus slug is dropped (not a real capability).
	if containsStr(matchedGuideSlugs(&Plan{Guides: []string{"not_a_guide"}}, ps), "not_a_guide") {
		t.Errorf("an unknown guide slug must be dropped, not delivered")
	}
}
