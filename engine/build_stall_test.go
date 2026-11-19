package engine

import (
	"context"
	"strings"
	"testing"

	"openfabrica/engine/enginetest"
	"openfabrica/providers/llm"
)

func TestBuildTurnEnd_DoesNotInjectFileRejectRepairForNonFileTools(t *testing.T) {
	w, pdb := scheduledTestWorker(t, nil)
	_, err := AddTasks(pdb, "run_1", []Task{{
		ID:     "t1",
		Title:  "Projects table",
		Intent: "Create projects table",
		Origin: TaskOriginPlan,
	}})
	if err != nil {
		t.Fatalf("add task: %v", err)
	}

	hook := w.buildWholeSpec(&Plan{}, "").OnTurnEnd
	inject, stop := hook(0, []llm.ToolCall{{
		ID:        "c1",
		Name:      "manage_schema",
		Arguments: `{"action":"create_table","table_name":"projects"}`,
	}}, 1)

	if stop != nil {
		t.Fatalf("unexpected stop: %+v", stop)
	}
	if strings.Contains(inject, "manage_files") || strings.Contains(inject, "REJECTED") {
		t.Fatalf("must not inject file-patch repair nudge for manage_schema failure, got %q", inject)
	}
}

func TestBuildStage_EmptyRepeatStallFailsInsteadOfShippingLive(t *testing.T) {
	badSchemaCall := enginetest.Calls(llm.ToolCall{
		ID:        "s1",
		Name:      "manage_schema",
		Arguments: `{"action":"create_table","table_name":"projects","view_name":"x"}`,
	})
	p := &enginetest.Provider{Steps: []enginetest.Step{
		badSchemaCall, badSchemaCall, badSchemaCall, badSchemaCall,
	}}
	w, _ := buildE2EWorker(t, p)
	run := &Run{ID: "run_empty_stall", Kind: RunKindBuild, Stage: StageBuild}
	if _, err := w.pdb.ExecWrite(`INSERT INTO of_runs (id, kind, status, stage) VALUES (?, 'build', 'running', 'build')`, run.ID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	_, err := AddTasks(w.pdb, run.ID, []Task{{
		ID:     "projects-table",
		Title:  "Projects table",
		Intent: "Create projects table",
		Origin: TaskOriginPlan,
	}})
	if err != nil {
		t.Fatalf("add task: %v", err)
	}

	out, err := (buildStage{}).Run(context.Background(), w, run)
	if err == nil {
		t.Fatalf("expected buildStage.Run to fail on empty repeat_stall, got out=%+v", out)
	}
	if !strings.Contains(err.Error(), "no deliverables") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildWholeSpec_BlocksFlowActionsUntilFlowGate(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(llm.ToolCall{
			ID:        "f1",
			Name:      "manage_testing",
			Arguments: `{"action":"write_flows","source":"export default { flows: [] }"}`,
		}),
		enginetest.Text("ok"),
	}}
	w, _ := buildE2EWorker(t, p)
	plan := &Plan{}

	buildSpec := w.buildWholeSpec(plan, "")
	if !buildSpec.BlockFlowActions {
		t.Fatal("buildWholeSpec must set BlockFlowActions=true")
	}
	flowSpec := w.flowPhaseSpec(plan, "")
	if flowSpec.BlockFlowActions {
		t.Fatal("flowPhaseSpec must clear BlockFlowActions=false")
	}
	if !strings.Contains(w.flowGateInstruction(""), "user-flows") {
		t.Fatal("flowGateInstruction must name the user-flows checklist item")
	}

	sess := w.newStageSession(StageBuild)
	buildSpec.OnTurnEnd = nil
	buildSpec.OnModelDone = nil
	_, err := w.runSpec(context.Background(), sess, buildSpec)
	if err != nil {
		t.Fatalf("runSpec: %v", err)
	}
	var count int
	_ = w.pdb.Reader().QueryRow(`SELECT COUNT(*) FROM of_files WHERE path = 'of_test.js'`).Scan(&count)
	if count != 0 {
		t.Fatalf("of_test.js must not be saved during pre-flow-gate build, got count=%d", count)
	}
}
