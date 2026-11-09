package engine

// Offline end-to-end: drive a full PLAN -> BUILD -> go-live run through the
// real worker pipeline with the scripted fake provider - no API key, no
// network, no Chrome (validators are cleared for the run). This is the
// closest thing to a build that runs in CI.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
	_ "openfabrica/tools/all" // register every tool category via init()
)

// planJSON is the fake planner's output: one page task.
// userFlowsStep is the flow-gate session's write_flows call: save a minimal of_test.js
// for the finished app. The gate adds no checklist item, so there is nothing to tick -
// the session ends when the model next stops (a plain-text turn follows this). Offline
// has no browser, so the save's replay is skipped, not red.
var userFlowsStep = enginetest.Calls(
	llm.ToolCall{ID: "cf1", Name: "manage_testing",
		Arguments: `{"action":"write_flows","source":"export default { flows: [{ name: \"home\", steps: [{ goto: \"/\" }, { expectText: \"Tip calculator\" }] }] }"}`},
)

const planJSON = `{
  "goal": "A one-page tip calculator.",
  "intro": "Here we go - a tip calculator.",
  "voice": "friendly and concise",
  "design": "clean, centered card on a soft background",
  "tasks": [
    {"id": "home-page", "title": "Home page", "intent": "Build the tip calculator page at /", "done_when": "/ renders the calculator", "paths": ["/"]}
  ]
}`

func TestOfflineEndToEndBuild(t *testing.T) {
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	defer gdb.Close()
	mgr := db.NewProjectDBManager(dir)
	defer mgr.CloseAll()

	// A minimal project row (llm_model_id is NOT NULL but unenforced by value).
	// user_flows on so the run exercises the real flow path (write_flows + the engine's
	// user-flows checklist item), which is the current build hotspot; off by default.
	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level, config)
		 VALUES (1, 'Tip Calc', 'a one-page tip calculator', 1, 'building', 1, 'off', '{"user_flows":true}')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Scripted provider: PLAN emits the plan JSON; the build session saves the page and
	// ticks its item (the page-probe nudge then fires once and the model acknowledges it,
	// ending the build); then the post-build flow gate writes of_test.js for the finished
	// app and stops. User flows are no longer a build checklist item - they are this gate.
	provider := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSON),
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`}),
		enginetest.Calls(llm.ToolCall{ID: "c2", Name: "manage_tasks",
			Arguments: `{"action":"complete","task_id":"home-page","result":"built the calculator page"}`}),
		// The page-probe nudge fires once the checklist resolves with no probe run; the
		// offline run has no browser, so the model just acknowledges it and the build stops.
		enginetest.Text("The page probe is skipped in this offline run."),
		// Post-build flow gate (user flows on + the project has a page): write of_test.js,
		// then a plain-text turn ends the gate (it adds no checklist item to resolve).
		userFlowsStep,
		enginetest.Text("Flows saved."),
	}}

	deps := &Deps{
		DB:               gdb,
		ProjectDBManager: mgr,
		ToolRegistry:     tools.DefaultRegistry(),
		ToolExecutor:     tools.NewExecutor(tools.DefaultRegistry()),
	}
	worker, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

	// No validators for the offline run (the runtime probe needs a server).
	validatorMu.Lock()
	saved := validatorSet
	validatorSet = nil
	validatorMu.Unlock()
	defer func() {
		validatorMu.Lock()
		validatorSet = saved
		validatorMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	worker.setState(StateBuilding)
	worker.drive(ctx)
	// Drain the async call logger before asserting: of_llm_calls is written by a
	// buffered channel plus one writer goroutine, so a count taken right after
	// the run races the writer on a slow machine.
	worker.dbLog.Close()

	// The run completed and the project went live.
	if got := worker.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live", got)
	}
	pdb, _ := mgr.Open(1)
	run, err := latestRun(pdb)
	if err != nil || run == nil {
		t.Fatalf("latest run: %v", err)
	}
	if run.Status != RunStatusCompleted || run.Stage != StageLive {
		t.Fatalf("run status=%s stage=%s, want completed/live", run.Status, run.Stage)
	}
	if run.PlanJSON == "" {
		t.Fatal("run carries no plan")
	}

	// The task drained.
	all, _ := LoadTasks(pdb.Reader())
	tasks := plannedTasks(all)
	if len(tasks) != 1 || tasks[0].Status != TaskDone {
		t.Fatalf("tasks = %+v, want one done task", tasks)
	}

	// Per-task cost instrumentation: draining the item wrote exactly one
	// of_run_log 'task' completion row, keyed by the task id and carrying its
	// title, which the admin cost view windows build-stage call cost onto.
	var taskLogCount int
	var taskPayload string
	_ = pdb.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(payload), '') FROM of_run_log
		 WHERE run_id = ? AND kind = 'task' AND ref = 'home-page'`, run.ID).Scan(&taskLogCount, &taskPayload)
	if taskLogCount != 1 {
		t.Fatalf("of_run_log task completion rows for home-page = %d, want 1", taskLogCount)
	}
	if !strings.Contains(taskPayload, "Home page") {
		t.Fatalf("task completion payload = %q, want it to carry the task title", taskPayload)
	}

	// The page artifact landed.
	var body string
	if err := pdb.QueryRow(
		"SELECT body FROM of_files WHERE kind='page' AND path='/'").Scan(&body); err != nil {
		t.Fatalf("page artifact missing: %v", err)
	}
	if body != "<h1>Tip calculator</h1>" {
		t.Fatalf("page body = %q", body)
	}

	// projects.mode flipped to live.
	var mode string
	if err := gdb.QueryRow("SELECT mode FROM projects WHERE id = 1").Scan(&mode); err != nil || mode != "live" {
		t.Fatalf("project mode = %q (%v), want live", mode, err)
	}

	// The platform favicon was synthesised for the page project.
	var favCount int
	_ = pdb.QueryRow(
		"SELECT COUNT(*) FROM of_files WHERE kind='file' AND path='favicon.svg' AND is_deleted=0").Scan(&favCount)
	if favCount != 1 {
		t.Fatalf("platform favicon missing (count=%d)", favCount)
	}
	// The go-live message carries the what's-inside overview.
	var overviewCount int
	_ = pdb.QueryRow(
		`SELECT COUNT(*) FROM of_chat_messages WHERE content LIKE '%What''s inside%'`).Scan(&overviewCount)
	if overviewCount < 1 {
		t.Fatal("go-live message missing the overview section")
	}

	// Tool calls persist on the brain assistant rows so a history reload can
	// redraw the tool cards the live stream showed. Each BUILD tool call (c1,
	// c2) must land as a non-NULL tool_calls entry paired with a role='tool'
	// result row keyed by the same id; a NULL tool_calls column would orphan the
	// results on reload.
	for _, id := range []string{"c1", "c2"} {
		var asstCount int
		_ = pdb.QueryRow(
			`SELECT COUNT(*) FROM of_chat_messages
			 WHERE session_id='brain' AND role='assistant'
			   AND tool_calls IS NOT NULL AND tool_calls LIKE '%"'||?||'"%'`, id).Scan(&asstCount)
		if asstCount < 1 {
			t.Fatalf("tool call %s not persisted on any assistant row", id)
		}
		var toolCount int
		_ = pdb.QueryRow(
			`SELECT COUNT(*) FROM of_chat_messages
			 WHERE session_id='brain' AND role='tool' AND tool_call_id=?`, id).Scan(&toolCount)
		if toolCount != 1 {
			t.Fatalf("tool result row for %s = %d, want 1", id, toolCount)
		}
	}

	// The LLM log marks no hits (first run is all fresh). Nine calls: the PLAN
	// guide-selection pre-pass, the brief-feature extraction, the PLAN call, the three
	// build calls (the last answers the page-probe nudge), the spec audit after the build,
	// and the two flow-gate calls (write_flows, then the plain-text turn that ends it).
	// The replay cache holds all but the audit, which always judges the code afresh.
	var cached int
	if err := pdb.QueryRow("SELECT COUNT(*) FROM of_llm_cache").Scan(&cached); err != nil || cached != 8 {
		t.Fatalf("replay cache rows = %d (%v), want 8", cached, err)
	}
	var calls, hits int
	_ = pdb.QueryRow("SELECT COUNT(*), COALESCE(SUM(cache_hit),0) FROM of_llm_calls WHERE source='engine'").Scan(&calls, &hits)
	if calls != 9 || hits != 0 {
		t.Fatalf("llm calls=%d hits=%d, want 9/0", calls, hits)
	}
}

// TestOfflineCrashResume pauses a build on a permanent provider error (the
// bad-API-key story), then "restarts" with a fresh worker and asserts the run
// resumes from its persisted stage and completes - PLAN is not re-run, the
// unfinished checklist item is picked back up. (Replay-cache hit semantics for the
// identical-request case are pinned at the unit layer in
// TestReplayCacheNewSegmentReplays; after a mid-task mutation the resumed
// request legitimately differs, so no hit is asserted here.)
func TestOfflineCrashResume(t *testing.T) {
	dir := t.TempDir()
	gdb, err := db.Open(filepath.Join(dir, "global.db"))
	if err != nil {
		t.Fatalf("open global db: %v", err)
	}
	defer gdb.Close()
	mgr := db.NewProjectDBManager(dir)
	defer mgr.CloseAll()
	if _, err := gdb.ExecWrite(
		`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level)
		 VALUES (1, 'Tip Calc', 'a one-page tip calculator', 1, 'building', 1, 'off')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	newTestWorker := func(p *enginetest.Provider) *Worker {
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
		w.providerOverride = staticProviderSource{rp: resolvedProvider{
			Provider: p, Model: "fake-model", ProviderID: 1}}
		return w
	}

	validatorMu.Lock()
	saved := validatorSet
	validatorSet = nil
	validatorMu.Unlock()
	defer func() {
		validatorMu.Lock()
		validatorSet = saved
		validatorMu.Unlock()
	}()

	// Run 1: PLAN succeeds, then BUILD's first call hits a permanent error
	// (bad API key) - the circuit breaker pauses the run immediately.
	crash := &enginetest.Provider{Steps: []enginetest.Step{
		selectNoGuides, noFeatures, // PLAN guide-selection pre-pass
		enginetest.Text(planJSON),
		enginetest.Fail(&llm.Error{Code: llm.CodeAuth, Message: "invalid api key"}),
	}}
	w1 := newTestWorker(crash)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w1.setState(StateBuilding)
	w1.drive(ctx)
	w1.dbLog.Close()
	if w1.State() != StatePaused {
		t.Fatalf("failed run should pause, state = %v", w1.State())
	}
	pdb, _ := mgr.Open(1)
	if run, _ := activeRun(pdb); run == nil || run.Stage != StageBuild {
		t.Fatalf("paused run should persist at stage build, got %+v", run)
	}

	// "Restart" (owner fixed the key, flipped mode back to building): a fresh
	// worker resumes from the persisted stage - PLAN is NOT re-run, the
	// unfinished checklist item is picked back up.
	resume := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`}),
		enginetest.Calls(llm.ToolCall{ID: "c2", Name: "manage_tasks",
			Arguments: `{"action":"complete","task_id":"home-page","result":"built the calculator page"}`}),
	}}
	w2 := newTestWorker(resume)
	defer w2.dbLog.Close()
	w2.setState(StateBuilding)
	w2.drive(ctx)

	if got := w2.State(); got != StateLive {
		t.Fatalf("resumed worker state = %v, want live", got)
	}
	run, _ := latestRun(pdb)
	if run == nil || run.Status != RunStatusCompleted {
		t.Fatalf("resumed run should complete, got %+v", run)
	}
	all, _ := LoadTasks(pdb.Reader())
	tasks := plannedTasks(all)
	if len(tasks) != 1 || tasks[0].Status != TaskDone {
		t.Fatalf("tasks = %+v, want one done task", tasks)
	}
	// The resume never re-ran PLAN: the fake's first call was the build task.
	if got := resume.Requests[0]; len(got.Tools) == 0 {
		t.Fatal("resume should re-enter BUILD (tool-bearing request), not PLAN")
	}
}
