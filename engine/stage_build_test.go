package engine

// Tests for the build stage's static tool set and the single-loop build's
// end-to-end session behaviour (build the whole plan in one session, tick the
// checklist, hard-stop when every item is done, then go live).

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/store"
	"openfabrica/tools"
)

// ---------------------------------------------------------------------------
// Unit: static tool set

func TestBuildPreloadTools(t *testing.T) {
	// Planner-declared tools ride along, deduped.
	got := buildPreloadTools(Task{Tools: []string{"manage_relay", "manage_files", "manage_relay"}}, true)
	count := map[string]int{}
	for _, n := range got {
		count[n]++
	}
	if count["manage_relay"] != 1 || count["manage_files"] != 1 {
		t.Fatalf("dedupe failed: %v", got)
	}

	// No declared tools: keyword inference fills in.
	got = buildPreloadTools(Task{Title: "Call page", Intent: "WebRTC video call with signaling and login"}, true)
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "manage_signaling") || !strings.Contains(joined, "manage_auth") {
		t.Fatalf("inference missed tools: %v", got)
	}
}

// TestBuildPreloadToolsUnionsInferenceWithDeclared pins the inference-union rule: a task that declared
// SOME tools still gets keyword inference for the ones it omitted. Before the
// union, inference fired only when task.Tools was empty, so a partially-declared
// task lost the omitted tool to a mid-session cache-busting find_tool.
func TestBuildPreloadToolsUnionsInferenceWithDeclared(t *testing.T) {
	// Declares manage_auth but the text also implies a realtime relay ("chat"/
	// "presence"/"websocket"). Both must be preloaded.
	got := buildPreloadTools(Task{
		Tools:  []string{"manage_auth"},
		Title:  "Live chat room",
		Intent: "websocket chat with presence, gated behind login",
	}, true)
	joined := "," + strings.Join(got, ",") + ","
	if !strings.Contains(joined, ",manage_auth,") {
		t.Fatalf("declared tool dropped: %v", got)
	}
	if !strings.Contains(joined, ",manage_relay,") {
		t.Fatalf("inference did not fill the omitted tool for a partially-declared task: %v", got)
	}
}

// TestBuildPromptDeferredCapabilitiesPin pins the two-sided contract between
// the BUILD system prompt and find_tool's description: BUILD deliberately
// carries NO '## Tool catalogue' (it renders once, in PLAN, where the planner
// declares each task's tool needs) and instead carries the '## Deferred
// capabilities' note - and find_tool's description points the model at exactly
// that note. Renaming either side without the other ships a description that
// references a section the BUILD model cannot find.
func TestBuildPromptDeferredCapabilitiesPin(t *testing.T) {
	sys := (&Worker{}).buildSystemPrompt(&Plan{})
	if !strings.Contains(sys, "## Deferred capabilities") {
		t.Fatalf("BUILD system prompt lost its '## Deferred capabilities' note - find_tool's description references it")
	}
	if strings.Contains(sys, "## Tool catalogue") {
		t.Fatalf("BUILD system prompt carries a '## Tool catalogue' - the catalogue is a PLAN/LIVE surface; if this is now intentional, update find_tool's description and mental-model.md to match")
	}
	if !strings.Contains(tools.FindToolDef().Description, "'## Deferred capabilities'") {
		t.Fatalf("find_tool's description no longer names the '## Deferred capabilities' note - keep it pointing at the section BUILD actually carries")
	}
}

// ---------------------------------------------------------------------------
// E2E: the single-loop build through the worker pipeline

// buildE2EWorker spins up the offline worker harness with a scripted provider.
func buildE2EWorker(t *testing.T, provider *enginetest.Provider) (*Worker, *db.ProjectDBManager) {
	t.Helper()
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	t.Cleanup(func() { gdb.Close() })
	mgr := db.NewProjectDBManager(dir)
	t.Cleanup(mgr.CloseAll)
	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level)
		 VALUES (1, 'Tip Calc', 'a one-page tip calculator', 1, 'building', 1, 'off')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     tools.DefaultRegistry(),
		ToolExecutor:     tools.NewExecutor(tools.DefaultRegistry()),
	}
	w, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	t.Cleanup(func() { w.dbLog.Close() })
	w.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	validatorMu.Lock()
	saved := validatorSet
	validatorSet = nil
	validatorMu.Unlock()
	t.Cleanup(func() {
		validatorMu.Lock()
		validatorSet = saved
		validatorMu.Unlock()
	})
	return w, mgr
}

func driveToCompletion(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w.setState(StateBuilding)
	w.drive(ctx)
}

var saveStep = enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
	Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`})

func completeStep(id string) enginetest.Step {
	return enginetest.Calls(llm.ToolCall{ID: id, Name: "manage_tasks",
		Arguments: `{"action":"complete","task_id":"home-page","result":"built the calculator page"}`})
}

// The single-loop build runs the whole plan in ONE session: the agent saves
// the artifacts, ticks each checklist item complete, and the done-checklist
// hard-stop ends the build. There is no per-item verification gate (the
// post-build validators are the net), so the agent's complete is taken at face
// value and the run goes live with the model-authored result recorded.
func TestSingleLoopBuildHappyPath(t *testing.T) {
	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSON),
		saveStep,
		completeStep("c2"),
	}}
	w, mgr := buildE2EWorker(t, provider)
	driveToCompletion(t, w)

	if got := w.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live", got)
	}
	pdb, _ := mgr.Open(1)
	all, _ := LoadTasks(pdb.Reader())
	tasks := plannedTasks(all)
	if len(tasks) != 1 || tasks[0].Status != TaskDone {
		t.Fatalf("tasks = %+v, want one done task", tasks)
	}
	if tasks[0].Result != "built the calculator page" {
		t.Fatalf("task result = %q, want the model-authored result", tasks[0].Result)
	}
}

// A build session that ends without completing every checklist item does NOT
// auto-resolve the open items: the stage simply proceeds to validation, which
// re-surfaces any real breakage as a fix-pass. Here the open item is left
// pending and the run still goes live.
func TestSingleLoopBuildNoAutoComplete(t *testing.T) {
	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSON),
		saveStep,
		enginetest.Text("all done here"), // clean text stop, no complete call
	}}
	w, mgr := buildE2EWorker(t, provider)
	driveToCompletion(t, w)

	if got := w.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live", got)
	}
	pdb, _ := mgr.Open(1)
	all, _ := LoadTasks(pdb.Reader())
	tasks := plannedTasks(all)
	if len(tasks) != 1 || tasks[0].Status != TaskPending {
		t.Fatalf("tasks = %+v, want the item left pending (no auto-complete)", tasks)
	}
}

// A zero-task checklist can never reach the all-resolved stop, so it must stop
// outright instead of running to the iteration cap.
func TestBuildTurnEndStopsOnEmptyChecklist(t *testing.T) {
	w, _ := buildE2EWorker(t, &enginetest.Provider{})
	msg, stop := w.buildTurnEnd()(0, nil, 0)
	if stop == nil || msg == "" {
		t.Fatalf("an empty checklist must stop the session, got stop=%v msg=%q", stop, msg)
	}
}

// A real split-game build saved 23 modules over 17 turns and completed nothing, so the
// owner's progress view sat at zero all build. Once enough writes land with no complete,
// the build gets ONE Track reminder; a build that completes as it goes never sees it.
func TestBuildTurnEndTrackNudge(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES (?, ?, '', 'pending', ?)`,
			fmt.Sprintf("t%d", i), fmt.Sprintf("T%d", i), i); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	save := llm.ToolCall{Name: "manage_files", Arguments: `{"action":"save","file_path":"/files/a.js"}`}
	twoSaves := []llm.ToolCall{save, save}

	hook := w.buildTurnEnd()
	for turn := 1; turn <= 2; turn++ {
		if inject, _ := hook(turn, twoSaves, 0); inject != "" {
			t.Fatalf("turn %d: no reminder before %d writes, got %q", turn, trackNudgeAfterSaves, inject)
		}
	}
	inject, stop := hook(3, twoSaves, 0)
	if stop != nil || inject != trackNudge {
		t.Fatalf("after %d writes and no complete the Track reminder must fire, got stop=%v inject=%q", trackNudgeAfterSaves, stop, inject)
	}
	if inject, _ := hook(4, twoSaves, 0); inject == trackNudge {
		t.Fatal("the Track reminder fires at most once per session")
	}

	// A build that completes as it goes never sees it.
	hook = w.buildTurnEnd()
	complete := llm.ToolCall{Name: "manage_tasks", Arguments: `{"action":"complete","task_id":"t0"}`}
	for turn := 1; turn <= 5; turn++ {
		if inject, _ := hook(turn, []llm.ToolCall{save, save, complete}, 0); inject == trackNudge {
			t.Fatalf("turn %d: a build that completes must not get the Track reminder", turn)
		}
	}
}

// A mid-build turn that batch-completes many items earns the re-check nudge
// exactly once; smaller batches and later turns run silent.
func TestBuildTurnEndBatchCompleteNudge(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	for i, status := range []string{"done", "done", "done", "done", "pending"} {
		if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES (?, ?, '', ?, ?)`,
			fmt.Sprintf("t%d", i), fmt.Sprintf("T%d", i), status, i); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	completes := make([]llm.ToolCall, batchCompleteNudgeAt)
	for i := range completes {
		completes[i] = llm.ToolCall{Name: "manage_tasks", Arguments: `{"action":"complete","task_id":"x"}`}
	}

	hook := w.buildTurnEnd()
	inject, stop := hook(3, completes, 0)
	if stop != nil || !strings.Contains(strings.ToLower(inject), "re-check") {
		t.Fatalf("a batch of %d completes with items remaining must earn the nudge, got stop=%v inject=%q",
			batchCompleteNudgeAt, stop, inject)
	}
	if inject, _ := hook(4, completes, 0); inject != "" {
		t.Fatalf("the nudge fires at most once per session, got %q", inject)
	}
}

// The failure: one turn closes EVERY remaining item and the build stops on
// the spot. That is the batch most in need of a re-check, and gating the nudge behind
// !allResolved made it the one batch that never got one. The turn must nudge, not stop.
func TestBuildTurnEndNudgesOnTotalBatchClose(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	// Every task already done, as they are when the closing turn's completes land.
	for i := 0; i < batchCompleteNudgeAt+1; i++ {
		if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES (?, ?, '', 'done', ?)`,
			fmt.Sprintf("t%d", i), fmt.Sprintf("T%d", i), i); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	completes := make([]llm.ToolCall, batchCompleteNudgeAt+1)
	for i := range completes {
		completes[i] = llm.ToolCall{Name: "manage_tasks", Arguments: `{"action":"complete","task_id":"x"}`}
	}

	hook := w.buildTurnEnd()
	inject, stop := hook(11, completes, 0)
	if stop != nil {
		t.Fatalf("a turn closing every item must nudge before stopping, got stop=%v", stop)
	}
	if !strings.Contains(inject, "test_page") {
		t.Fatalf("the closing nudge must ask for the end-of-build probe, got %q", inject)
	}
	// The very next turn stops: the nudge is one-shot, so this costs one turn, not a loop.
	inject, stop = hook(12, nil, 0)
	if stop == nil || stop.outcome != EndDone || inject != checklistCompleteStop {
		t.Fatalf("the turn after the nudge must stop, got stop=%v inject=%q", stop, inject)
	}
}

// A resolved checklist with no batch this turn still stops immediately: the nudge is
// for batches, and a build that closed its items one at a time has nothing to re-check.
func TestBuildTurnEndStopsWithoutABatch(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES ('t0','T0','','done',0)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	one := []llm.ToolCall{{Name: "manage_tasks", Arguments: `{"action":"complete","task_id":"t0"}`}}
	if inject, stop := w.buildTurnEnd()(5, one, 0); stop == nil || inject != checklistCompleteStop {
		t.Fatalf("a single close on a resolved checklist must stop, got stop=%v inject=%q", stop, inject)
	}
}

// The ship-broken case: the closing turn issues three patches, one lands (an added
// import) and two are rejected, and the build would stop and go live with a
// file importing a name nothing used. A turn with a rejected edit must finish the
// repair before any stop.
func TestBuildTurnEndRefusesToStopOnRejectedWrites(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES ('t0','T0','','done',0)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	patch := []llm.ToolCall{{Name: "manage_files", Arguments: `{"action":"patch","file_path":"a.js"}`}}

	hook := w.buildTurnEnd()
	inject, stop := hook(29, patch, 2)
	if stop != nil {
		t.Fatalf("a turn with rejected edits must not end the build, got stop=%v", stop)
	}
	if !strings.Contains(inject, "REJECTED") || !strings.Contains(inject, "patches") {
		t.Fatalf("the repair ask must name the rejection and the one-call rule, got %q", inject)
	}
	// A clean turn afterwards stops normally: the repair ask is not a loop.
	if inject, stop := hook(30, patch, 0); stop == nil || inject != checklistCompleteStop {
		t.Fatalf("a clean turn on a resolved checklist must stop, got stop=%v inject=%q", stop, inject)
	}
}

// The repair ask is bounded: a model that keeps failing cannot hold the build open,
// so after the cap the checklist stop wins and the validators own what is left.
func TestBuildTurnEndRepairAskIsBounded(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES ('t0','T0','','done',0)`); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	patch := []llm.ToolCall{{Name: "manage_files", Arguments: `{"action":"patch","file_path":"a.js"}`}}
	hook := w.buildTurnEnd()
	for i := 0; i < maxRejectRepairTurns; i++ {
		if _, stop := hook(i, patch, 1); stop != nil {
			t.Fatalf("repair turn %d must not stop", i)
		}
	}
	if _, stop := hook(99, patch, 1); stop == nil {
		t.Fatal("past the cap the build must stop rather than loop on failing edits")
	}
}

// warmFixEligible: a fix-pass continues the prior round's transcript only when
// that round ended on its own terms with real context headroom; everything else
// cold-reseeds. Each rejected case is one way a carried transcript would be a
// bad base: a pause/failure mid-thought, an empty carry, or a tail so close to
// rollover the warm pass would immediately pay the reseed anyway.
// The failure: the model narrates "everything is in place", emits no tool
// call, and the session ends with an item still pending - which the go-live report then
// tells the owner was never built, though its files were on disk. The stop must be
// turned into one re-prompt, and exactly one: a second stop ends the segment.
func TestBuildModelDoneRepromptsOnceOnOpenChecklist(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	for i, status := range []string{"done", "pending"} {
		if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES (?, ?, '', ?, ?)`,
			fmt.Sprintf("t%d", i), fmt.Sprintf("Item %d", i), status, i); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}

	hook := w.buildModelDone()
	inject := hook()
	if !strings.Contains(inject, "Item 1") {
		t.Fatalf("the re-prompt must name the open item, got %q", inject)
	}
	if again := hook(); again != "" {
		t.Fatalf("the re-prompt fires at most once, got %q", again)
	}
}

// A blocked item is resolved work the owner is already told about, so it must not hold
// the session open; neither must a fully done checklist.
func TestBuildModelDoneIgnoresResolvedChecklists(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	for i, status := range []string{"done", "blocked"} {
		if _, err := pdb.ExecWrite(`INSERT INTO of_tasks (id, title, intent, status, position) VALUES (?, ?, '', ?, ?)`,
			fmt.Sprintf("t%d", i), fmt.Sprintf("Item %d", i), status, i); err != nil {
			t.Fatalf("seed task: %v", err)
		}
	}
	if inject := w.buildModelDone()(); inject != "" {
		t.Fatalf("done and blocked items are resolved, got %q", inject)
	}
}

// TestPlanNeedsBackbone pins the data-work predicate against the plan shapes it was
// derived from. The backbone tier is three tool schemas that ride the cached prefix and are
// re-read on every turn, so a realtime or media build must not carry them, and a table build
// must. The `paths` shapes below are taken verbatim from real plans: a realtime endpoint task
// owns a bare resource name too, which is why the declared tool is what separates it from a
// plain table task.
// A real run preloaded manage_auth and inlined the auth_contract guide for an app whose
// plan locked `auth strategy = none`: a task said "everything is public, no-auth", and
// the hyphen made "auth" a whole-word keyword hit. The locked decision vetoes inference;
// a tool the planner DECLARES still loads.
func TestNoAuthDecisionVetoesInferredAuth(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	plan := &Plan{
		Decisions: []PlanDecision{{Topic: "auth strategy", Choice: "none"}},
		Tasks: []PlanTaskSpec{
			{Title: "Files + drops tables", Intent: "No user tables; everything is public, no-auth.", Paths: []string{"files"}},
			{Title: "Home", Paths: []string{"/"}},
		},
	}
	if containsName(buildAllTools(plan), "manage_auth") {
		t.Error("a no-auth plan must not preload manage_auth from a keyword hit")
	}
	if planGuideSlugs(plan, ps)["auth_contract"] {
		t.Error("a no-auth plan must not inline the auth_contract guide from a keyword hit")
	}

	// Without the decision the keyword still preloads (inference is generous by design).
	plan.Decisions = nil
	if !containsName(buildAllTools(plan), "manage_auth") {
		t.Error("with no auth decision, the keyword hit should still preload manage_auth")
	}

	// A declared tool always wins over the veto.
	plan.Decisions = []PlanDecision{{Topic: "Authentication", Choice: "no login"}}
	plan.Tasks[0].Tools = []string{"manage_auth"}
	if !containsName(buildAllTools(plan), "manage_auth") {
		t.Error("a planner-declared manage_auth must survive the no-auth veto")
	}

	// A real auth choice vetoes nothing.
	if decisionVetoedTools(&Plan{Decisions: []PlanDecision{{Topic: "auth strategy", Choice: "email + password"}}}) != nil {
		t.Error("an actual auth strategy must not veto the auth tools")
	}
}

func TestPlanNeedsBackbone(t *testing.T) {
	cases := []struct {
		name string
		plan *Plan
		want bool
	}{
		{"no plan keeps the tier", nil, true},
		{"empty plan keeps the tier", &Plan{}, true},
		{
			"realtime game: bare resource owned by a declared tool",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Tick endpoint", Paths: []string{"grove"}, Tools: []string{"manage_tick"}},
				{Title: "App spine", Paths: []string{"/", "/files/grove.js"}},
				{Title: "Shell", Paths: []string{"default"}},
			}},
			false,
		},
		{
			"webrtc: signaling endpoint is not data work",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Signaling", Paths: []string{"signal"}, Tools: []string{"manage_signaling"}},
				{Title: "Call page", Paths: []string{"/", "/files/app.call.js"}},
			}},
			false,
		},
		{
			"plain table task with no tool of its own",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Files table", Paths: []string{"files"}},
				{Title: "Upload", Paths: []string{"upload"}, Tools: []string{"manage_uploads"}},
			}},
			true,
		},
		{
			// The plan frame tells the planner not to list the backbone, so a task that
			// builds a table AND declares another tool says so only in its text.
			"table task that also declares a non-backbone tool",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Messages table + chat relay", Intent: "A messages table persisting chat history, and a relay endpoint.",
					Paths: []string{"messages", "chat"}, Tools: []string{"manage_relay"}},
				{Title: "Chat page", Paths: []string{"/"}},
			}},
			true,
		},
		{
			// The table task names its table only in its text, while other tasks own routes.
			"table task with empty paths beside routed pages",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Posts table + CRUD API", Intent: "Create a posts table and a create_api endpoint on it."},
				{Title: "Home", Paths: []string{"/"}},
			}},
			true,
		},
		{
			"planner declared a backbone tool outright",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Habits", Paths: []string{"/"}, Tools: []string{"manage_schema"}},
			}},
			true,
		},
		{
			"pages only, no resource of any kind",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Shell", Paths: []string{"default"}},
				{Title: "Home", Paths: []string{"/", "/files/home.js"}},
				{Title: "About", Paths: []string{"/about"}},
			}},
			false,
		},
		{
			// manage_auth works over an EXISTING user table, so the auth task's
			// bare path is table work even though the task declares a tool.
			"auth task: user table owned by a task declaring manage_auth",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Auth", Paths: []string{"users"}, Tools: []string{"manage_auth"}},
				{Title: "Login page", Paths: []string{"/login"}},
			}},
			true,
		},
		{
			"no task declares paths or edits: shape unknown, keep the tier",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Build the tracker", Intent: "habit tracker with streaks"},
			}},
			true,
		},
		{
			"edit-only update plan is a known shape",
			&Plan{Tasks: []PlanTaskSpec{
				{Title: "Add testimonials", Edits: []string{"/"}},
			}},
			false,
		},
	}
	for _, c := range cases {
		if got := planNeedsBackbone(c.plan); got != c.want {
			t.Errorf("%s: planNeedsBackbone = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestBuildAllToolsDropsBackboneForRealtimePlan is the end-to-end of the predicate: the
// resolved static tool set for a realtime plan must not carry the data-backbone schemas,
// and the same plan with a plain table task must.
func TestBuildAllToolsDropsBackboneForRealtimePlan(t *testing.T) {
	realtime := &Plan{Tasks: []PlanTaskSpec{
		{Title: "Tick endpoint", Paths: []string{"grove"}, Tools: []string{"manage_tick"}},
		{Title: "App spine", Paths: []string{"/", "/files/grove.js"}},
	}}
	joined := "," + strings.Join(buildAllTools(realtime), ",") + ","
	for _, n := range tools.BuildBackboneToolNames {
		if strings.Contains(joined, ","+n+",") {
			t.Errorf("realtime plan preloaded %s; the backbone tier must stay deferred there", n)
		}
	}
	if !strings.Contains(joined, ",manage_files,") {
		t.Errorf("core tier dropped: %s", joined)
	}

	withTable := &Plan{Tasks: append(append([]PlanTaskSpec{}, realtime.Tasks...),
		PlanTaskSpec{Title: "Scores table", Paths: []string{"scores"}})}
	joined = "," + strings.Join(buildAllTools(withTable), ",") + ","
	for _, n := range tools.BuildBackboneToolNames {
		if !strings.Contains(joined, ","+n+",") {
			t.Errorf("table plan did not preload %s: %s", n, joined)
		}
	}
}

// sweepValidator reports one deterministic error on each of its first `failing`
// calls (every call when failing is 0) and counts the calls.
type sweepValidator struct{ calls, failing int }

func (*sweepValidator) Name() string { return "sweep" }
func (s *sweepValidator) Validate(context.Context, *sql.DB, string, string) []Issue {
	s.calls++
	if s.failing > 0 && s.calls > s.failing {
		return nil
	}
	return []Issue{{Severity: store.SeverityError, Category: "js_syntax", File: "app.js", Deterministic: true, Message: "app.js line 3: broken"}}
}

// A finding the build cannot fix reopens it for a repair round, is tested again, and
// when the round made no progress ships in the go-live report as a known issue. The repair item
// it became does not linger on the checklist as unbuilt work.
func TestBuildRepairsFindingsThenShipsWhatRemains(t *testing.T) {
	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
		saveStep,
		completeStep("c2"),
	}}
	w, mgr := buildE2EWorker(t, provider)
	v := &sweepValidator{}
	validatorMu.Lock()
	validatorSet = []Validator{v}
	validatorMu.Unlock()
	driveToCompletion(t, w)

	if got := w.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live", got)
	}
	// One round that leaves the finding exactly as it was is not worth another: the sweep
	// ran after the build and after that round only.
	if want := 2; v.calls != want {
		t.Fatalf("validators ran %d times, want %d (after the build and after the one unproductive repair round)", v.calls, want)
	}
	var sawRepair bool
	for _, req := range provider.Requests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "[mode: repair]") && strings.Contains(m.Content, "app.js line 3: broken") {
				sawRepair = true
			}
		}
	}
	if !sawRepair {
		t.Fatal("a repair round must show the model the finding under [mode: repair]")
	}
	pdb, _ := mgr.Open(1)
	var n int
	_ = pdb.Reader().QueryRow(
		`SELECT COUNT(*) FROM of_chat_messages WHERE content LIKE '%deployed, with known issues%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("the go-live report must say it shipped with known issues, found %d such messages", n)
	}
	_ = pdb.Reader().QueryRow(`SELECT COUNT(*) FROM of_tasks WHERE origin = ? AND status <> ?`, TaskOriginRepair, TaskDone).Scan(&n)
	if n != 0 {
		t.Fatalf("open repair items must be dropped once their findings ship as known issues, %d left", n)
	}
}

// A finding the repair round fixes is gone from the final sweep: one round, two
// sweeps, and the project goes live clean.
func TestBuildRepairRoundClearsAFinding(t *testing.T) {
	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures,
		enginetest.Text(planJSON),
		saveStep,
		completeStep("c2"),
	}}
	w, mgr := buildE2EWorker(t, provider)
	v := &sweepValidator{failing: 1}
	validatorMu.Lock()
	validatorSet = []Validator{v}
	validatorMu.Unlock()
	driveToCompletion(t, w)

	if v.calls != 2 {
		t.Fatalf("validators ran %d times, want 2 (after the build and after the one repair round)", v.calls)
	}
	pdb, _ := mgr.Open(1)
	var n int
	_ = pdb.Reader().QueryRow(
		`SELECT COUNT(*) FROM of_chat_messages WHERE content LIKE '%known issues%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("a repaired build ships clean, found %d known-issue messages", n)
	}
}

// A live wake that adds a page no flow visits is asked once for that flow; pages that
// already have one, the 404 page, dynamic routes and pages from before the wake are not.
func TestLiveFlowCoverageAsksOnceForANewPage(t *testing.T) {
	w, mgr := buildE2EWorker(t, &enginetest.Provider{})
	// User-flow tests are off by default; the nudge only exists for an owner who turned them on.
	if _, err := w.deps.DB.ExecWrite(`UPDATE projects SET config = '{"user_flows":true}' WHERE id = 1`); err != nil {
		t.Fatalf("turn flows on: %v", err)
	}
	pdb, err := mgr.Open(1)
	if err != nil {
		t.Fatalf("open pdb: %v", err)
	}
	add := func(kind, path, body, created string) {
		t.Helper()
		if _, err := pdb.ExecWrite(`INSERT INTO of_files (kind, path, body, scope, created_at) VALUES (?, ?, ?, 'page', ?)`,
			kind, path, body, created); err != nil {
			t.Fatalf("insert %s: %v", path, err)
		}
	}
	add("page", "/old", "<p>old</p>", "2020-01-01 00:00:00")
	add("test", "of_test.js", `export default { flows: [{ name: "a", steps: [{ goto: "/covered" }] }] }`, "2020-01-01 00:00:00")
	hook := w.liveFlowCoverage(time.Now().Add(-time.Minute))
	if got := hook(); got != "" {
		t.Fatalf("nothing was added in this wake, got %q", got)
	}
	add("page", "/covered", "<p>c</p>", time.Now().UTC().Format("2006-01-02 15:04:05"))
	add("page", "/404", "<p>nf</p>", time.Now().UTC().Format("2006-01-02 15:04:05"))
	add("page", "/items/:slug", "<p>i</p>", time.Now().UTC().Format("2006-01-02 15:04:05"))
	if got := hook(); got != "" {
		t.Fatalf("a covered page, the 404 and a dynamic route need no flow, got %q", got)
	}
	add("page", "/social", "<p>s</p>", time.Now().UTC().Format("2006-01-02 15:04:05"))
	got := hook()
	if !strings.Contains(got, "/social") || strings.Contains(got, "/covered") {
		t.Fatalf("the uncovered page must be named, got %q", got)
	}
	if again := hook(); again != "" {
		t.Fatalf("the nudge fires at most once, got %q", again)
	}
}
