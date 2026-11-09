package engine

import (
	"slices"
	"testing"
)

// A planner writes the realtime-model choice as free text. Every reader compares it
// exactly, so before canonicalization "authoritative reducer" silently skipped the
// reducer-contract build seed and the realtime tool pruning.
func TestNormalizeRealtimeChoice(t *testing.T) {
	for in, want := range map[string]string{
		"authoritative":               "authoritative",
		"Authoritative reducer":       "authoritative",
		"tick - continuous state":     "tick",
		"relay":                       "relay",
		"matchmaking + authoritative": "matchmaking",
		"manage_signaling":            "webrtc",
		"WebRTC":                      "webrtc",
		"peer to peer":                "",
		"none":                        "none",
		"None - single user":          "none",
		"n/a":                         "none",
		"offline":                     "none",
	} {
		if got := normalizeRealtimeChoice(in); got != want {
			t.Errorf("normalizeRealtimeChoice(%q) = %q, want %q", in, got, want)
		}
	}

	p := &Plan{Decisions: []PlanDecision{{Topic: "Realtime model", Choice: "Authoritative reducer"}}}
	normalizeRealtimeChoices(p)
	if !planLocksAuthoritative(p.Decisions) {
		t.Fatalf("decision %+v did not normalize to a locked authoritative model", p.Decisions[0])
	}
	if w := planCheckWarnings(&Plan{Decisions: []PlanDecision{{Topic: "realtime model", Choice: "peer to peer"}}}); len(w) == 0 {
		t.Error("an unrecognized realtime-model choice produced no plan warning")
	}
}

// "multiplayer" preloads every realtime model tool; a locked decision keeps only its own
// (plus whatever the planner declared).
func TestLockedRealtimeChoicePrunesOtherModels(t *testing.T) {
	plan := &Plan{
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "tick"}},
		Tasks:     []PlanTaskSpec{{ID: "net", Title: "Multiplayer arena", Intent: "multiplayer movement for every player"}},
	}
	names := buildAllTools(plan)
	if !slices.Contains(names, "manage_tick") {
		t.Fatalf("locked tick model lost manage_tick: %v", names)
	}
	for _, other := range []string{"manage_relay", "manage_authoritative", "manage_matchmaking"} {
		if slices.Contains(names, other) {
			t.Errorf("locked tick model still preloads %s: %v", other, names)
		}
	}
}

// planWritesReducer is the decision-independent signal: a task that declared
// manage_authoritative writes a reducer even when the locked model is something else
// (a matchmaking lobby that plays on a separate authoritative endpoint), so the
// room-reducer contract still rides the build seed.
func TestPlanWritesReducer(t *testing.T) {
	mm := &Plan{
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "matchmaking"}},
		Tasks: []PlanTaskSpec{
			{ID: "lobby", Title: "Matchmaking queue", Tools: []string{"manage_matchmaking"}},
			{ID: "game", Title: "Match reducer", Tools: []string{"manage_authoritative"}},
		},
	}
	if planLocksAuthoritative(mm.Decisions) {
		t.Fatal("a matchmaking decision must not report as locked-authoritative")
	}
	if !planWritesReducer(mm) {
		t.Errorf("a task declaring manage_authoritative must trigger planWritesReducer: tools %v", planExtraTools(mm))
	}

	tick := &Plan{
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "tick"}},
		Tasks:     []PlanTaskSpec{{ID: "world", Title: "Shared world", Tools: []string{"manage_tick"}}},
	}
	if planWritesReducer(tick) {
		t.Errorf("a tick-only plan must not report as writing a reducer: tools %v", planExtraTools(tick))
	}
}

// An offline single-user app records the realtime model the plan stage asks
// for as "none". That is a real answer - no warning - and it locks NO realtime tool, so
// a keyword-inferred one ("chat" in a support chatbot) is not carried every turn.
func TestRealtimeChoiceNone(t *testing.T) {
	plan := &Plan{
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "none"}},
		Tasks:     []PlanTaskSpec{{ID: "help", Title: "Help chat", Intent: "a chat with the FAQ bot"}},
	}
	if w := planCheckWarnings(plan); len(w) != 0 {
		t.Errorf("realtime model none must not warn, got %v", w)
	}
	for _, n := range buildAllTools(plan) {
		if isRealtimeModelTool(n) {
			t.Errorf("realtime model none still preloads %s", n)
		}
	}
	plan.Tasks[0].Tools = []string{"manage_relay"}
	if !slices.Contains(buildAllTools(plan), "manage_relay") {
		t.Error("a planner-declared realtime tool must stay")
	}
}
