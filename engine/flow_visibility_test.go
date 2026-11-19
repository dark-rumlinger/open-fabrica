package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"openfabrica/engine/enginetest"
	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

func testingActionEnum(t *testing.T, defs []llm.ToolDef) []string {
	t.Helper()
	for _, d := range defs {
		if d.Name != "manage_testing" {
			continue
		}
		props, _ := d.Parameters["properties"].(map[string]any)
		act, _ := props["action"].(map[string]any)
		enum, _ := act["enum"].([]string)
		return enum
	}
	t.Fatal("manage_testing not found in tool defs")
	return nil
}

func assertNoUserFlowMentions(t *testing.T, label, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, needle := range []string{"user_flows", "user flow", "of_test.js", "write_flows", "test_flows"} {
		if strings.Contains(lower, needle) {
			t.Errorf("%s must not mention %q when user flows are hidden/disabled", label, needle)
		}
	}
}

func TestUserFlowsDisabledHidesAllReferences(t *testing.T) {
	w, _ := buildE2EWorker(t, &enginetest.Provider{})
	if _, err := w.deps.DB.ExecWrite(`UPDATE projects SET config = '{"user_flows":false}' WHERE id = 1`); err != nil {
		t.Fatalf("disable user_flows: %v", err)
	}
	plan := &Plan{Tasks: []PlanTaskSpec{{ID: "home", Title: "Home", Paths: []string{"/"}}}}

	// 1. PLAN hides user_flows guide.
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	planIn := planAssembleInput(ps, w.deps.ToolRegistry, llm.Capabilities{}, nil)
	if !planIn.HiddenGuides["user_flows"] {
		t.Fatal("PLAN must hide user_flows guide")
	}

	// 2. BUILD & REPAIR hide user_flows guide and omit write_flows/test_flows.
	buildSpec := w.buildWholeSpec(plan, "")
	assertNoUserFlowMentions(t, "buildSystemPrompt", buildSpec.System)
	assertNoUserFlowMentions(t, "buildSharedSeed", w.buildSharedSeed(plan))
	if !buildSpec.ExtraHiddenGuides["user_flows"] {
		t.Fatal("buildWholeSpec must hide user_flows guide")
	}
	s := &session{deps: w.deps, logger: w.logger, projectID: w.projectID}
	buildEnum := testingActionEnum(t, s.resolveTools(&buildSpec))
	if slices.Contains(buildEnum, "write_flows") || slices.Contains(buildEnum, "test_flows") {
		t.Fatalf("build manage_testing must omit flow actions, got %v", buildEnum)
	}

	// 3. LIVE hides user_flows guide, omits write_flows/test_flows, and omits flows prose.
	liveSpec := w.liveWakeSpec("hello")
	assertNoUserFlowMentions(t, "liveSystemPrompt", liveSpec.System)
	if !liveSpec.ExtraHiddenGuides["user_flows"] {
		t.Fatal("liveWakeSpec must hide user_flows guide when disabled")
	}
	liveEnum := testingActionEnum(t, s.resolveTools(&liveSpec))
	if slices.Contains(liveEnum, "write_flows") || slices.Contains(liveEnum, "test_flows") {
		t.Fatalf("live manage_testing must omit flow actions when disabled, got %v", liveEnum)
	}

	// 4. runFlowGate is a no-op and inserts no user-flows task.
	issues, err := w.runFlowGate(context.Background(), plan, "")
	if err != nil || len(issues) != 0 {
		t.Fatalf("runFlowGate when disabled: issues=%v err=%v", issues, err)
	}
	if tasks, _ := LoadTasks(w.pdb.Reader()); len(tasks) != 0 {
		t.Fatalf("disabled runFlowGate must not insert tasks, got %+v", tasks)
	}
}

func TestUserFlowsEnabledSeparatesBuildFromFlowGateAndCreatesTask(t *testing.T) {
	p := &enginetest.Provider{Steps: []enginetest.Step{
		enginetest.Calls(
			llm.ToolCall{ID: "f1", Name: "manage_testing", Arguments: `{"action":"write_flows","source":"export default { flows: [{ name: \"home\", steps: [{ goto: \"/\" }] }] }"}`},
			llm.ToolCall{ID: "f2", Name: "manage_tasks", Arguments: `{"action":"complete","task_id":"user-flows","result":"done"}`},
		),
	}}
	w, _ := buildE2EWorker(t, p)
	if _, err := w.deps.DB.ExecWrite(`UPDATE projects SET config = '{"user_flows":true}' WHERE id = 1`); err != nil {
		t.Fatalf("enable user_flows: %v", err)
	}
	plan := &Plan{Tasks: []PlanTaskSpec{{ID: "home", Title: "Home", Paths: []string{"/"}}}}
	s := &session{deps: w.deps, logger: w.logger, projectID: w.projectID}

	// During main BUILD, flow actions are omitted and user_flows is hidden.
	buildSpec := w.buildWholeSpec(plan, "")
	buildEnum := testingActionEnum(t, s.resolveTools(&buildSpec))
	if slices.Contains(buildEnum, "write_flows") || slices.Contains(buildEnum, "test_flows") {
		t.Fatalf("pre-gate build must omit flow actions, got %v", buildEnum)
	}
	if !buildSpec.ExtraHiddenGuides["user_flows"] {
		t.Fatal("pre-gate build must hide user_flows guide")
	}

	// During flow_gate, regular probe actions are omitted and ONLY write_flows/test_flows remain.
	planCopy := clonePlanForFlows(plan)
	ensureUserFlowsTask(planCopy)
	flowSpec := w.flowPhaseSpec(planCopy, "")
	flowEnum := testingActionEnum(t, s.resolveTools(&flowSpec))
	if len(flowEnum) != 2 || !slices.Contains(flowEnum, "write_flows") || !slices.Contains(flowEnum, "test_flows") {
		t.Fatalf("flow_gate manage_testing must expose only write_flows and test_flows, got %v", flowEnum)
	}
	if flowSpec.ExtraHiddenGuides["user_flows"] {
		t.Fatal("flow_gate must allow user_flows guide")
	}

	// Verify LIVE sees flow actions when enabled.
	liveSpec := w.liveWakeSpec("hello")
	liveEnum := testingActionEnum(t, s.resolveTools(&liveSpec))
	if !slices.Contains(liveEnum, "write_flows") || !slices.Contains(liveEnum, "test_flows") {
		t.Fatalf("live manage_testing must expose flow actions when enabled, got %v", liveEnum)
	}

	// Running runFlowGate creates the user-flows task item in of_tasks and completes it.
	if _, err := w.runFlowGate(context.Background(), plan, ""); err != nil {
		t.Fatalf("runFlowGate: %v", err)
	}
	tasks, err := LoadTasks(w.pdb.Reader())
	if err != nil {
		t.Fatalf("LoadTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != userFlowsTaskID || tasks[0].Status != TaskDone {
		b, _ := json.Marshal(tasks)
		t.Fatalf("expected completed user-flows task in of_tasks, got %s", b)
	}
	// Ensure manage_guides hides user_flows when HiddenGuides includes it.
	res, raw, err := tools.NewExecutor(tools.DefaultRegistry()).Invoke(context.Background(), w.deps.toolEnv(), tools.Call{
		Name:      "manage_guides",
		Args:      map[string]any{"action": "recall", "slug": "user_flows"},
		ProjectID: w.projectID,
		Project:   w.pdb,
		Decorate: func(ctx *tools.ToolContext) {
			ctx.HiddenGuides = map[string]bool{"user_flows": true}
		},
	})
	if err != nil || (res != nil && res.Success) || !strings.Contains(raw, "unavailable") {
		t.Fatalf("expected manage_guides recall user_flows to be hidden, got res=%+v raw=%s err=%v", res, raw, err)
	}
}
