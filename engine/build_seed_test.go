package engine

import (
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/tools"
)

// The look is committed at PLAN as values, and for a while those values reached the PWA
// manifest and the post-build owner chat but never the build that writes the CSS. A build
// that cannot see them re-derives the whole look from one prose paragraph, which is how
// distinct briefs land on one template.
func TestBuildIntentSeedCarriesDesignSystem(t *testing.T) {
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	plan := &Plan{
		Goal:   "a tip calculator",
		Design: "dense, warm, unhurried",
		DesignSystem: &PlanDesignSystem{
			Colors:      PlanDesignColors{Primary: "#3b1f6e", Bg: "#f6f1e7", Surface: "#ffffff", Text: "#1b1a17"},
			FontBody:    "Source Serif 4",
			FontDisplay: "Space Grotesk",
			Motion:      "160ms ease-out",
		},
	}
	intent := w.buildIntentSeed(plan)

	for _, want := range []string{"#3b1f6e", "#f6f1e7", "Source Serif 4", "Space Grotesk", "160ms ease-out"} {
		if !strings.Contains(intent, want) {
			t.Errorf("the intent seed drops %q, so BUILD re-invents the look the plan already committed", want)
		}
	}
	if !strings.Contains(intent, "global stylesheet") {
		t.Error("the token block never says where the values go, so they read as trivia rather than as the source for :root")
	}
	// The block is the cached prefix of every task of the build, so a re-render that
	// differs by a byte costs the cache on every turn.
	if intent != w.buildIntentSeed(plan) {
		t.Error("buildIntentSeed is not byte-stable across renders")
	}
}

// An all-empty block is what a provider that ignores ResponseSchema emits. Rendering its
// heading spends the cached prefix on nothing and tells the build a look was committed
// when none was.
func TestBuildIntentSeedOmitsEmptyDesignSystem(t *testing.T) {
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	for name, plan := range map[string]*Plan{
		"absent": {Goal: "a tip calculator"},
		"shell":  {Goal: "a tip calculator", DesignSystem: &PlanDesignSystem{}},
	} {
		if got := w.buildIntentSeed(plan); strings.Contains(got, "Design system (") {
			t.Errorf("%s: the seed renders an empty token block", name)
		}
	}
}

// The reducer wire contract is an extended guide section, so only a plan that
// locked the authoritative model carries it in the seed; tick and relay builds
// must not pay for it on every turn.
func TestReducerContractRidesSeedOnlyWhenAuthoritative(t *testing.T) {
	w := &Worker{deps: &Deps{ToolRegistry: tools.DefaultRegistry()}}
	const marker = "The room reducer (`manage_authoritative set_reducer`)"

	auth := &Plan{Decisions: []PlanDecision{{Topic: "realtime model", Choice: "authoritative"}}}
	shared := w.buildSharedSeed(auth)
	if !strings.Contains(shared, marker) {
		t.Error("authoritative-locked plan seed lacks the room-reducer contract")
	}
	if strings.Contains(shared, "{{contract:") {
		t.Error("seed carries an unresolved contract token")
	}

	tick := &Plan{Decisions: []PlanDecision{{Topic: "realtime model", Choice: "tick"}}}
	if strings.Contains(w.buildSharedSeed(tick), marker) {
		t.Error("tick-locked plan seed carries the room-reducer contract")
	}
	if strings.Contains(w.buildSharedSeed(&Plan{}), marker) {
		t.Error("decision-free plan seed carries the room-reducer contract")
	}
}

// A task owning entry + store with task-level exports: the checklist's import
// scaffold must point siblings at the STORE module (the un-edited owned
// module), never the entry, since that is where the save gate requires the
// symbols to live.
func TestChecklistScaffoldHomesExportsInStore(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	if _, err := AddTasks(pdb, "run_1", []Task{
		{ID: "spine", Title: "Spine", Paths: []string{"/", "/files/game.js", "/files/game.state.js"},
			Exports: []string{"setState(patch)", "getState()"}},
		{ID: "hud", Title: "HUD", Paths: []string{"/files/game.hud.js"}, Edits: []string{"/files/game.js"}},
	}); err != nil {
		t.Fatalf("AddTasks: %v", err)
	}
	out := renderChecklist(pdb.Reader())
	if !strings.Contains(out, "from 'game.state'") {
		t.Errorf("scaffold should import from the store module game.state, got:\n%s", out)
	}
	if strings.Contains(out, "from 'game'") {
		t.Errorf("scaffold must not point siblings at the entry, got:\n%s", out)
	}
}
