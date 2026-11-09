package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"openfabrica/db"
	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// TestOfflineExpandThenBuild drives a full EXPAND -> PLAN -> BUILD run with
// ambition on: the expanded brief persists on the run, PLAN's prompt carries
// it (not the raw description), and the run completes.
func TestOfflineExpandThenBuild(t *testing.T) {
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
		 VALUES (1, 'Tip Calc', 'tip calc', 1, 'building', 1, 'high')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	const expandedBrief = "A polished one-page tip calculator with bill input, percentage presets, and split-by-people."
	provider := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Text(expandedBrief), // EXPAND
		selectNoGuides, noFeatures,     // PLAN guide-selection pre-pass
		enginetest.Text(planJSON), // PLAN
		enginetest.Calls(llm.ToolCall{ID: "c1", Name: "manage_files",
			Arguments: `{"action":"save","kind":"page","file_path":"/","body":"<h1>Tip calculator</h1>","title":"Tip calculator"}`}),
		enginetest.Calls(llm.ToolCall{ID: "c2", Name: "manage_tasks",
			Arguments: `{"action":"complete","task_id":"home-page","result":"done"}`}),
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
	defer worker.dbLog.Close()
	worker.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}

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

	if got := worker.State(); got != StateLive {
		t.Fatalf("worker state = %v, want live", got)
	}
	pdb, _ := mgr.Open(1)
	run, _ := latestRun(pdb)
	if run == nil || run.Status != RunStatusCompleted {
		t.Fatalf("run = %+v, want completed", run)
	}
	if run.ExpandedBrief != expandedBrief {
		t.Fatalf("expanded brief not persisted: %q", run.ExpandedBrief)
	}

	// PLAN's user message (request 2) carries the EXPANDED brief, not the raw
	// description.
	planReq := provider.Requests[1]
	found := false
	for _, m := range planReq.Messages {
		if contains(m.Content, expandedBrief) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("PLAN prompt does not carry the expanded brief")
	}

	// The expand stage emitted the owner-facing card.
	var n int
	_ = pdb.QueryRow(
		`SELECT COUNT(*) FROM of_chat_messages WHERE content LIKE '**Expanded brief**%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("expanded-brief chat card count = %d, want 1", n)
	}
}

// TestExpandSelfSkips pins the skip conditions: update runs and
// already-expanded runs forward straight to PLAN without an LLM call.
func TestExpandSelfSkips(t *testing.T) {
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
		 VALUES (1, 'P', 'desc', 1, 'building', 1, 'high')`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	provider := &enginetest.Provider{}
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
	defer w.dbLog.Close()
	w.providerOverride = staticProviderSource{rp: resolvedProvider{
		Provider: provider, Model: "fake-model", ProviderID: 1}}
	pdb, _ := mgr.Open(1)

	// Update run: skipped.
	updateRun, err := createRun(pdb, RunKindUpdate, StageExpand, "make it blue")
	if err != nil {
		t.Fatalf("create update run: %v", err)
	}
	w.run = updateRun
	out, err := expandStage{}.Run(context.Background(), w, updateRun)
	if err != nil || resolveNext(StageExpand, out) != StagePlan {
		t.Fatalf("update run: out=%+v err=%v, want next=plan", out, err)
	}

	// Already-expanded run: skipped.
	buildRun, err := createRun(pdb, RunKindBuild, StageExpand, "")
	if err != nil {
		t.Fatalf("create build run: %v", err)
	}
	_ = buildRun.setExpandedBrief(pdb, "already expanded")
	w.run = buildRun
	out, err = expandStage{}.Run(context.Background(), w, buildRun)
	if err != nil || resolveNext(StageExpand, out) != StagePlan {
		t.Fatalf("expanded run: out=%+v err=%v, want next=plan", out, err)
	}

	if provider.CallCount() != 0 {
		t.Fatalf("skip paths must not call the provider, calls = %d", provider.CallCount())
	}
}

// TestResolveRunEntryStage pins the ambition gate: off enters at PLAN,
// anything else at EXPAND.
func TestResolveRunEntryStage(t *testing.T) {
	for _, tc := range []struct {
		ambition string
		want     string
	}{
		{"off", StagePlan},
		{"medium", StageExpand},
		{"high", StageExpand},
	} {
		dir := t.TempDir()
		gdb, err := db.Open(filepath.Join(dir, "global.db"))
		if err != nil {
			t.Fatalf("open global db: %v", err)
		}
		mgr := db.NewProjectDBManager(dir)
		if _, err := gdb.ExecWrite(
			`INSERT INTO projects (id, name, description, published, mode, llm_model_id, ambition_level)
			 VALUES (1, 'P', 'desc', 1, 'building', 1, ?)`, tc.ambition); err != nil {
			t.Fatalf("insert project: %v", err)
		}
		deps := &Deps{DB: gdb, ProjectDBManager: mgr, ToolRegistry: tools.DefaultRegistry(), ToolExecutor: tools.NewExecutor(tools.DefaultRegistry())}
		w, err := newWorker(deps, 1, newLLMLimiter(2), context.Background())
		if err != nil {
			t.Fatalf("new worker: %v", err)
		}
		run, fresh, err := w.resolveRun()
		if err != nil {
			t.Fatalf("resolve run: %v", err)
		}
		if !fresh {
			t.Errorf("ambition %s: first resolve should create a fresh run", tc.ambition)
		}
		if run.Stage != tc.want {
			t.Errorf("ambition %s: entry stage = %s, want %s", tc.ambition, run.Stage, tc.want)
		}
		w.dbLog.Close()
		mgr.CloseAll()
		gdb.Close()
	}
}

// TestAmbitionDirectiveOffFallsBackToLow pins the off->low alignment shared
// with AmbitionTaskBudget: EXPAND only sees "off" on a resumed run whose
// ambition was lowered mid-flight, and the directive must then keep the
// expansion lean rather than inflating the brief.
func TestAmbitionDirectiveOffFallsBackToLow(t *testing.T) {
	if got, want := ambitionDirective(AmbitionOff), expandDirectives[AmbitionLow]; got != want {
		t.Errorf("ambitionDirective(off) = %q, want low's directive", firstWords(got, 4))
	}
}
