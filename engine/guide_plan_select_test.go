package engine

// Tests for the LLM-picked PLAN guide selection (selectPlanGuideSlugs). The
// invariant that matters: the picker's slugs drive which guides render, and its
// per-guide routing rules come from front-matter rather than the prompt; every
// picker miss - an empty brief, a provider that can never do structured output, a
// transient call failure, an unparseable reply - falls back to offering every
// guide's decision facet, so a plan is never blocked and never told to lock
// decisions it was shown none of. A change request unions in the existing plan's
// structured signals, and rival architectures resolve to one. See guide_prefetch.go.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"openfabrica/engine/enginetest"
	"openfabrica/pack"
)

// selectNoGuides is the scripted reply for the PLAN guide-selection pre-pass
// (added before PLAN for any brief long enough to route from): it selects no
// guides, so pipeline tests that don't exercise a capability guide can prepend it
// without changing what the plan produces.
var selectNoGuides = enginetest.Text(`{"guides":[]}`)

// noFeatures is the scripted reply for the brief-feature extraction that follows the
// guide pre-pass on a fresh plan: no features, so the coverage check stays off and the
// plan a test scripts is taken as written.
var noFeatures = enginetest.Text(`{"features":[]}`)

func capCount(t *testing.T) int {
	t.Helper()
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	return len(pack.Capabilities(ps))
}

// TestParseGuideSelection covers the pure parse: valid slugs kept, unknown
// dropped, malformed JSON errors (so the caller falls back to keyword routing).
func TestParseGuideSelection(t *testing.T) {
	valid := map[string]bool{"realtime_signaling": true, "payments": true}
	got, err := parseGuideSelection(`{"guides":["realtime_signaling","bogus_guide","payments"]}`, valid)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0] != "realtime_signaling" || got[1] != "payments" {
		t.Errorf("want [realtime_signaling payments] (bogus dropped), got %v", got)
	}
	// Fence-wrapped reply: some models ignore structured output and wrap the JSON
	// in ```json ... ```. jsonrepair.Repair strips the fence so the picker still parses.
	fenced, err := parseGuideSelection("```json\n{\"guides\":[\"payments\"]}\n```", valid)
	if err != nil {
		t.Fatalf("fenced parse: %v", err)
	}
	if len(fenced) != 1 || fenced[0] != "payments" {
		t.Errorf("want [payments] from fenced reply, got %v", fenced)
	}
	if _, err := parseGuideSelection("not json at all", valid); err == nil {
		t.Error("malformed JSON must error so the caller falls back to keyword routing")
	}
}

// TestGuideSelectionIndexListsDecisionGuides confirms the picker's index shows
// the decision-bearing guides, so the model can choose them.
func TestGuideSelectionIndexListsDecisionGuides(t *testing.T) {
	ps, _ := pack.Embedded()
	idx := guideSelectionIndex(pack.Capabilities(ps))
	for _, slug := range []string{"realtime_signaling", "payments", "llm_endpoints"} {
		if !strings.Contains(idx, slug) {
			t.Errorf("guide selection index missing decision-bearing guide %q", slug)
		}
	}
}

// TestGuideSelectionSystemHasPrecisionGuards confirms the picker prompt keeps the
// generic evidence gate, and that every evidence-gated guide's own routing prose
// actually reaches the picker. Without the gate the picker's include-when-unsure
// bias pulls large guide bodies into every plan that merely mentions a game or a
// tool, and the false positives can run to a fifth of the plan prompt. The gate is
// generic here and per-guide in front-matter, so this asserts the mechanism rather
// than a list of slugs, which is what lets a new guide bring its own rule.
func TestGuideSelectionSystemHasPrecisionGuards(t *testing.T) {
	ps, _ := pack.Embedded()
	caps := pack.Capabilities(ps)
	sys := guideSelectionSystem(caps)
	for _, want := range []string{"explicit evidence", "(needs explicit evidence)", "Use when", "Not for", "any language"} {
		if !strings.Contains(sys, want) {
			t.Errorf("picker prompt missing generic evidence-gate text %q", want)
		}
	}
	gated := 0
	for _, g := range caps {
		if !g.EvidenceGated {
			continue
		}
		gated++
		if !strings.Contains(sys, "- "+g.Slug+" (needs explicit evidence)") {
			t.Errorf("guide %q is evidence_gated but the picker index does not mark it", g.Slug)
		}
		if !strings.Contains(sys, g.UseWhen) {
			t.Errorf("guide %q use_when does not reach the picker prompt", g.Slug)
		}
		if !strings.Contains(sys, g.NotWhen) {
			t.Errorf("guide %q not_when does not reach the picker prompt", g.Slug)
		}
	}
	// The gate only works if something is gated. A corpus that gated nothing would
	// pass every assertion above while taxing each plan with the whole catalogue.
	if gated == 0 {
		t.Error("no guide declares evidence_gated - the picker's precision gate is inert")
	}
}

// TestSelectPlanGuideSlugs_LLMPicks: a real brief drives the picker call, and its
// chosen slugs are exactly what inlines.
func TestSelectPlanGuideSlugs_LLMPicks(t *testing.T) {
	ps, _ := pack.Embedded()
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text(`{"guides":["realtime_signaling","payments"]}`),
	}}
	w, _ := scheduledTestWorker(t, p)
	want, ran := w.selectPlanGuideSlugs(context.Background(),
		"A multiplayer game lobby with an online store and Stripe checkout.", &Plan{}, ps)
	if !ran {
		t.Fatal("expected the picker to run for a real brief")
	}
	if !want["realtime_signaling"] || !want["payments"] {
		t.Errorf("picker's slugs must be selected, got %v", sortedSet(want))
	}
	if want["e2ee"] || want["voxel_worlds"] {
		t.Errorf("unpicked guides must not inline, got %v", sortedSet(want))
	}
	if p.CallCount() != 1 {
		t.Errorf("selection is one call, got %d", p.CallCount())
	}
}

// TestSelectPlanGuideSlugs_NonEnglishBrief: a brief in another language reaches the
// picker verbatim and the system prompt tells it to judge by meaning, so the owner's
// language never decides which guides inline.
func TestSelectPlanGuideSlugs_NonEnglishBrief(t *testing.T) {
	ps, _ := pack.Embedded()
	brief := "Een webshop met betalingen via Stripe en een live chat voor klanten."
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text(`{"guides":["payments"]}`),
	}}
	w, _ := scheduledTestWorker(t, p)
	want, ran := w.selectPlanGuideSlugs(context.Background(), brief, &Plan{}, ps)
	if !ran || !want["payments"] {
		t.Fatalf("picker's slugs must be selected for a non-English brief, ran=%v got %v", ran, sortedSet(want))
	}
	req := p.Requests[0]
	if !strings.Contains(req.System, "any language") {
		t.Error("picker prompt must say the brief may be in any language")
	}
	sent := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content, brief) {
			sent = true
		}
	}
	if !sent {
		t.Error("the brief must reach the picker unchanged")
	}
}

// TestSelectPlanGuideSlugs_ShortBriefRoutes: even a terse brief runs the picker -
// the 512-token micro-call is far cheaper than inlining every guide body, and a
// short plain brief ("todo app") should end up with NO guides, not all of them.
func TestSelectPlanGuideSlugs_ShortBriefRoutes(t *testing.T) {
	ps, _ := pack.Embedded()
	p := &enginetest.Provider{Steps: []enginetest.Step{selectNoGuides}}
	w, _ := scheduledTestWorker(t, p)
	want, ran := w.selectPlanGuideSlugs(context.Background(), "todo app", &Plan{}, ps)
	if !ran {
		t.Fatal("a terse brief must still call the picker")
	}
	if len(want) != 0 {
		t.Errorf("picker chose no guides, so none must inline, got %v", sortedSet(want))
	}
	if p.CallCount() != 1 {
		t.Errorf("selection is one call, got %d", p.CallCount())
	}
}

// TestSelectPlanGuideSlugs_EmptyBriefInlinesAll: an empty brief carries nothing
// to route from, so it skips the call and offers every guide (safe fallback).
func TestSelectPlanGuideSlugs_EmptyBriefInlinesAll(t *testing.T) {
	ps, _ := pack.Embedded()
	p := &enginetest.Provider{}
	w, _ := scheduledTestWorker(t, p)
	want, ran := w.selectPlanGuideSlugs(context.Background(), "   \n ", &Plan{}, ps)
	if ran {
		t.Error("an empty brief must not call the picker")
	}
	if len(want) != capCount(t) {
		t.Errorf("empty brief must inline all %d guides, got %d", capCount(t), len(want))
	}
	if p.CallCount() != 0 {
		t.Errorf("empty brief must make no call, got %d", p.CallCount())
	}
}

// TestSelectPlanGuideSlugs_NoStructuredOutputOffersEveryGuide: a provider that can
// never force JSON makes no call and offers the planner every guide's decision
// facet. Selecting nothing would be cheaper, but the always-on index carries no
// `## Decision:` block, so a planner told to lock every decision it is shown would
// lock none - and these are exactly the small local models least able to recover.
// The facets are a fraction of the cores, so offering all of them is affordable
// (the facet is a fraction of the core's rendered size).
func TestSelectPlanGuideSlugs_NoStructuredOutputOffersEveryGuide(t *testing.T) {
	ps, _ := pack.Embedded()
	p := &enginetest.Provider{NoStructuredOutput: true}
	w, _ := scheduledTestWorker(t, p)
	want, ran := w.selectPlanGuideSlugs(context.Background(),
		"A multiplayer voxel game with a Stripe checkout.", &Plan{}, ps)
	if ran {
		t.Error("no-structured-output provider must not call the picker")
	}
	if p.CallCount() != 0 {
		t.Errorf("the fallback must make no LLM call, got %d", p.CallCount())
	}
	if len(want) != capCount(t) {
		t.Errorf("fallback must offer all %d guides, got %d: %v", capCount(t), len(want), sortedSet(want))
	}
	// Rival architectures are NOT resolved here: with no picker reply there is no
	// evidence to resolve on, and the plan frame carries the smooth-vs-blocky rule.
	// Dropping one by default would hide the cube world from a blocky brief.
	if !want["voxel_worlds"] || !want["heightfield_terrain"] {
		t.Errorf("the unrouted fallback must offer both terrain rivals, got %v", sortedSet(want))
	}
}

// TestSelectPlanGuideSlugs_FallbackOnUnparseable: an unparseable reply falls back
// to the full catalogue and reports the picker did not effectively run. The plan
// must never be blocked or silently under-served by a malformed micro-call reply.
func TestSelectPlanGuideSlugs_FallbackOnUnparseable(t *testing.T) {
	ps, _ := pack.Embedded()
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text("sorry, no JSON here")}}
	w, _ := scheduledTestWorker(t, p)
	want, ran := w.selectPlanGuideSlugs(context.Background(),
		"A large collaborative realtime whiteboard with many features to build.", &Plan{}, ps)
	if ran {
		t.Error("an unparseable reply should report ran=false")
	}
	if !want["realtime_signaling"] {
		t.Errorf("the fallback must still offer the realtime guide, got %v", sortedSet(want))
	}
	if len(want) != capCount(t) {
		t.Errorf("an unparseable reply must offer all %d guides, got %d", capCount(t), len(want))
	}
}

// TestSelectPlanGuideSlugs_FallbackOnCallFailure: a transient picker failure falls
// back to the full catalogue, unioned with the existing plan's signals.
func TestSelectPlanGuideSlugs_FallbackOnCallFailure(t *testing.T) {
	ps, _ := pack.Embedded()
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Fail(errors.New("scripted hiccup"))}}
	w, _ := scheduledTestWorker(t, p)
	existing := &Plan{Tasks: []PlanTaskSpec{{Title: "checkout", Intent: "hosted checkout", Tools: []string{"manage_payments"}}}}
	want, ran := w.selectPlanGuideSlugs(context.Background(),
		"Show live scores for local matches.", existing, ps)
	if ran {
		t.Error("a failed picker call should report ran=false")
	}
	if !want["payments"] {
		t.Errorf("existing plan's payments signal must be unioned in, got %v", sortedSet(want))
	}
	if len(want) != capCount(t) {
		t.Errorf("a failed picker call must offer all %d guides, got %d", capCount(t), len(want))
	}
}

// TestSelectPlanGuideSlugs_UnionsExistingPlan: a change request keeps a guide the
// existing plan's structured signals imply even when the picker returns none.
func TestSelectPlanGuideSlugs_UnionsExistingPlan(t *testing.T) {
	ps, _ := pack.Embedded()
	p := &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text(`{"guides":[]}`)}}
	w, _ := scheduledTestWorker(t, p)
	existing := &Plan{Tasks: []PlanTaskSpec{{Title: "checkout", Intent: "hosted checkout", Tools: []string{"manage_payments"}}}}
	want, ran := w.selectPlanGuideSlugs(context.Background(),
		"Change request: tweak the checkout button copy and the accent color.", existing, ps)
	if !ran {
		t.Fatal("picker should run for this brief")
	}
	if !want["payments"] {
		t.Errorf("existing plan's payments signal must be unioned in, got %v", sortedSet(want))
	}
}

// TestSelectPlanGuideSlugs_GameFeelSurvivesFirstPlan: game_feel anchors on a
// locked vanilla-canvas decision, but a first plan has locked nothing yet, so the
// picker's choice must stand. A plan that already settled the topic on something
// else still drops it.
func TestSelectPlanGuideSlugs_GameFeelSurvivesFirstPlan(t *testing.T) {
	ps, _ := pack.Embedded()
	pick := func() *enginetest.Provider {
		return &enginetest.Provider{Steps: []enginetest.Step{enginetest.Text(`{"guides":["game_feel"]}`)}}
	}
	w, _ := scheduledTestWorker(t, pick())
	want, ran := w.selectPlanGuideSlugs(context.Background(), "A small arcade game drawn on a canvas.", &Plan{}, ps)
	if !ran || !want["game_feel"] {
		t.Fatalf("a first plan must keep the picked game_feel, got ran=%v %v", ran, sortedSet(want))
	}

	settled := &Plan{Decisions: []PlanDecision{{Topic: "rendering approach", Choice: "dom only"}}}
	w, _ = scheduledTestWorker(t, pick())
	want, _ = w.selectPlanGuideSlugs(context.Background(), "A small arcade game drawn on a canvas.", settled, ps)
	if want["game_feel"] {
		t.Errorf("a settled non-canvas rendering decision must still drop game_feel, got %v", sortedSet(want))
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
