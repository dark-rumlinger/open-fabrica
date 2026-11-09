package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"openfabrica/db"
	"openfabrica/tools"
)

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

// A locked tick/authoritative decision settles the realtime wire tool, so the
// keyword-inferred manage_relay schema must not ride the preload; a planner-
// declared manage_relay (a second, relay-only endpoint) always survives.
func TestLockedRealtimeDropsInferredRelay(t *testing.T) {
	task := PlanTaskSpec{ID: "net", Title: "Multiplayer chat", Intent: "players share a room", Tools: []string{"manage_tick"}}
	has := func(names []string, n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}

	unlocked := &Plan{Tasks: []PlanTaskSpec{task}}
	if !has(buildAllTools(unlocked), "manage_relay") {
		t.Error("without a locked decision the relay inference must stand (a miss costs a find_tool)")
	}

	tick := &Plan{Tasks: []PlanTaskSpec{task}, Decisions: []PlanDecision{{Topic: "realtime model", Choice: "tick"}}}
	if has(buildAllTools(tick), "manage_relay") {
		t.Error("tick-locked plan still preloads keyword-inferred manage_relay")
	}

	declared := PlanTaskSpec{ID: "notify", Title: "Notifications", Intent: "live notifications", Tools: []string{"manage_relay"}}
	mixed := &Plan{Tasks: []PlanTaskSpec{task, declared}, Decisions: []PlanDecision{{Topic: "realtime model", Choice: "tick"}}}
	if !has(buildAllTools(mixed), "manage_relay") {
		t.Error("planner-declared manage_relay was dropped by the locked-decision gate")
	}

	relay := &Plan{Tasks: []PlanTaskSpec{task}, Decisions: []PlanDecision{{Topic: "realtime model", Choice: "relay"}}}
	if !has(buildAllTools(relay), "manage_relay") {
		t.Error("relay-locked plan lost the relay tool")
	}
}

// dedupeRepeatRejection collapses identical failed results within one turn to a
// stub naming the first call, keeping the code so triage still counts each
// rejection truthfully.
func TestDedupeRepeatRejection(t *testing.T) {
	seen := map[string]string{}
	body := `{"success":false,"code":"missing_arg","error":"missing required argument: action"}`

	first, stubbed := dedupeRepeatRejection(seen, "call_1", "manage_function", body)
	if stubbed || first != body {
		t.Fatalf("first occurrence must pass through, got stubbed=%v body=%q", stubbed, first)
	}
	stub, stubbed := dedupeRepeatRejection(seen, "call_2", "manage_function", body)
	if !stubbed {
		t.Fatal("identical second failure should be stubbed")
	}
	if !strings.Contains(stub, "call_1") || !strings.Contains(stub, `"missing_arg"`) {
		t.Errorf("stub should reference the first call and keep the code, got %s", stub)
	}
	// A different tool with the same body keeps its own first full error.
	if _, stubbed := dedupeRepeatRejection(seen, "call_3", "manage_api", body); stubbed {
		t.Error("a different tool must not be stubbed on its first failure")
	}
	// A different body on the same tool is a new error, not a repeat.
	other := `{"success":false,"code":"missing_arg","error":"missing required argument: name"}`
	if _, stubbed := dedupeRepeatRejection(seen, "call_4", "manage_function", other); stubbed {
		t.Error("a different error body must not be stubbed")
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

// collectAnswers is bounded to the newest 20 answers, kept in chronological
// order, so the block cannot grow without limit over a project's life.
func TestCollectAnswersBounded(t *testing.T) {
	mgr := db.NewProjectDBManager(t.TempDir())
	t.Cleanup(mgr.CloseAll)
	pdb, err := mgr.Create(1)
	if err != nil {
		t.Fatalf("create project db: %v", err)
	}
	w := &Worker{pdb: pdb}

	for i := 1; i <= 25; i++ {
		res, err := pdb.ExecWrite(`INSERT INTO of_questions (question, status) VALUES (?, 'answered')`, fmt.Sprintf("q%02d", i))
		if err != nil {
			t.Fatalf("insert question: %v", err)
		}
		qid, _ := res.LastInsertId()
		if _, err := pdb.ExecWrite(`INSERT INTO of_answers (question_id, answer) VALUES (?, ?)`, qid, fmt.Sprintf("a%02d", i)); err != nil {
			t.Fatalf("insert answer: %v", err)
		}
	}

	lines := strings.Split(w.collectAnswers(), "\n")
	if len(lines) != 20 {
		t.Fatalf("got %d lines, want the newest 20", len(lines))
	}
	if !strings.HasPrefix(lines[0], "q06:") {
		t.Errorf("oldest kept line should be q06, got %q", lines[0])
	}
	if !strings.HasPrefix(lines[19], "q25:") {
		t.Errorf("newest line should be q25, got %q", lines[19])
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
