package engine

// The user-flow gate: the final post-build phase. After the build and the repair loop
// have settled the app, and only when the owner turned user-flow tests on, one focused
// model session writes of_test.js for the FINISHED app and fixes what the replay
// reveals, then a single flow_tests validation gives the go-live verdict. Running flows
// last - against the settled app, after every other check - is what makes the owner see
// one arc (build -> checks -> fixes -> flows -> "it's live") instead of the model
// declaring victory and the engine then finding more to do.
//
// The gate works on an EPHEMERAL plan copy that carries the user-flows task: that copy
// drives the build frame's `when:flows` guidance and the user_flows guide prefetch for
// this one session, without adding a checklist item to the DB or persisting the plan -
// so a crash-resume re-enters a clean build (no flow task) and simply re-runs the gate
// (idempotent: of_test.js already saved and green means the session finishes fast, and
// the per-run replay budget bounds any re-replays).

import (
	"context"
	"database/sql"

	"openfabrica/pack"
	"openfabrica/probe"
	"openfabrica/providers/llm"
	"openfabrica/store"
)

// flowGateRound tags the gate's edit stats in the run log, apart from the build (0) and
// the numbered repair rounds.
const flowGateRound = 1000

// clonePlanForFlows returns a shallow copy with its own Tasks slice, so appending the
// user-flows task for the gate never mutates the stored plan.
func clonePlanForFlows(p *Plan) *Plan {
	if p == nil {
		return &Plan{}
	}
	cp := *p
	cp.Tasks = append([]PlanTaskSpec(nil), p.Tasks...)
	return &cp
}

// flowGateInstruction is the gate session's task message. The build frame's `when:flows`
// block (on for this session, because the ephemeral plan carries the flow task) and the
// user_flows guide carry the how-to; inlining the writing-flows section directly avoids a
// separate recall round-trip (and prevents models from looping on `manage_guides` recall).
func (w *Worker) flowGateInstruction(ownerAnswer string) string {
	writingFlows := w.flowGateWritingFlowsSection()
	recallHint := "after recalling the user_flows guide's writing-flows section (`manage_guides action=recall slug=user_flows section=writing-flows`). "
	if writingFlows != "" {
		recallHint = "using the `writing-flows` reference inlined below (no need to recall `user_flows`). "
	}
	msg := "[mode: build]\n\n"
	if ownerAnswer != "" {
		msg += "The owner answered your question(s):\n" + ownerAnswer + "\n\n"
	}
	msg += "The project is built and every fix testing found is done. One checklist item remains: `user-flows` (the user-flow tests).\n\n" +
		"Write of_test.js - the journeys a real person takes through THIS finished project - with " +
		"manage_testing action=write_flows, " + recallHint +
		"Only user-flow actions (`write_flows`, `test_flows`) are active here. " +
		"If all flows pass, mark `user-flows` complete (`manage_tasks action=complete task_id=user-flows`) and stop — do not call action=test_flows. " +
		"If any fail, fix what they reveal, then replay once with action=test_flows to check the fixes together - not after each single fix. " +
		"Keep it to at most 5 flows. For a red flow, follow the user_flows guide. Complete `user-flows` and stop when the flows pass, or " +
		"after 3 replays in a row that get no closer to green; the post-build check replays once more and reports anything still red."
	if writingFlows != "" {
		msg += "\n\n## Writing flows reference (`user_flows` / `writing-flows`)\n\n" + writingFlows
	}
	return msg
}

func (w *Worker) flowGateWritingFlowsSection() string {
	for _, g := range pack.Capabilities(w.effectivePacks()) {
		if g.Slug == "user_flows" {
			if sec := g.Section("writing-flows"); sec != nil {
				return pack.TranscludeContracts(sec.Body)
			}
			break
		}
	}
	return ""
}

// flowPhaseSpec is the gate session's spec: the build system prompt and tools (over the
// ephemeral plan copy, so `when:flows` and the guide prefetch fire), but with the flow
// task message and a turn-end that lets the model stop on its own rather than on "every
// checklist item resolved" - the gate adds no checklist item.
func (w *Worker) flowPhaseSpec(plan *Plan, ownerAnswer string) SessionSpec {
	spec := w.buildWholeSpec(plan, "")
	spec.BlockFlowActions = false
	spec.OmitToolActions = testingOmitRegularProbes
	spec.ExtraHiddenGuides = nil
	spec.MaxIterations = repairMaxIterations
	spec.OnTurnEnd = w.withOwnerInbox(func(int, []llm.ToolCall, int) (string, *ending) {
		w.publishTaskQueue()
		if taskResolved(w.pdb.Reader(), userFlowsTaskID) {
			return checklistCompleteStop, &endChecklistComplete
		}
		return "", nil
	})
	nudged := false
	spec.OnModelDone = func() string {
		if taskResolved(w.pdb.Reader(), userFlowsTaskID) || nudged {
			return ""
		}
		nudged = true
		if store.ArtifactExists(w.pdb.Reader(), probe.FlowKind, probe.FlowPath) {
			return "Complete the `user-flows` checklist item (`manage_tasks action=complete task_id=user-flows`) before stopping."
		}
		return "The `user-flows` checklist item is still open. Save `of_test.js` with `manage_testing action=write_flows` and complete `user-flows`."
	}
	seedFn := func() ([]llm.Message, error) {
		return seed{
			owner:  w.ownerContext() + w.ownerMemoryBlock(),
			shared: w.buildSharedSeed(plan),
			stable: w.buildIntentSeed(plan),
			task:   w.flowGateInstruction(ownerAnswer),
		}.messages(), nil
	}
	spec.Seed, _ = seedFn()
	spec.Reseed = seedFn
	return spec
}

// runFlowGate runs the final user-flow phase and returns the flow findings to merge into
// the go-live sweep. A no-op (nil) when the owner turned user-flow tests off or the
// project has no pages or API to replay against.
func (w *Worker) runFlowGate(ctx context.Context, plan *Plan, ownerAnswer string) ([]Issue, error) {
	if !w.userFlowsEnabled() {
		return nil, nil
	}
	planCopy := clonePlanForFlows(plan)
	if !ensureUserFlowsTask(planCopy) {
		return nil, nil // neither pages nor an API: nothing to replay
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	w.ensureFlowGateTask(planCopy)
	w.publishMessage("Now writing and replaying the user-flow tests.")
	sess := w.newStageSession(StageBuild)
	res, err := w.runSpec(ctx, sess, w.flowPhaseSpec(planCopy, ownerAnswer))
	if w.run != nil {
		w.runlog.editStats(w.run.ID, flowGateRound, res)
	}
	if w.settlePausedSession(res, err) {
		return nil, errStagePaused
	}
	if err != nil {
		return nil, err
	}
	return w.runFlowValidator(ctx), nil
}

// runFlowValidator runs just the flow_tests validator once, for the authoritative
// go-live verdict after the gate session.
func (w *Worker) runFlowValidator(ctx context.Context) []Issue {
	for _, v := range validators() {
		if v.Name() == flowTestsValidatorName {
			return w.runValidatorList(ctx, []Validator{v})
		}
	}
	return nil
}

// ensureFlowGateTask inserts the user-flows checklist item at the end of of_tasks if it
// is not already present, and publishes the updated queue to the UI.
func (w *Worker) ensureFlowGateTask(plan *Plan) {
	if w.pdb == nil {
		return
	}
	existing, err := LoadTasks(w.pdb.Reader())
	if err != nil {
		return
	}
	for _, t := range existing {
		if t.ID == userFlowsTaskID {
			w.publishTaskQueue()
			return
		}
	}
	var spec *PlanTaskSpec
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == userFlowsTaskID {
			spec = &plan.Tasks[i]
			break
		}
	}
	if spec == nil {
		return
	}
	runID := ""
	if w.run != nil {
		runID = w.run.ID
	}
	if _, err := AddTasks(w.pdb, runID, []Task{{
		ID:       spec.ID,
		RunID:    runID,
		Title:    spec.Title,
		Intent:   spec.Intent,
		DoneWhen: spec.DoneWhen,
		Status:   TaskPending,
		Deps:     spec.Deps,
		Origin:   TaskOriginPlan,
	}}); err != nil {
		w.logger.Warn("flow_gate: failed to insert user-flows task", "error", err)
		return
	}
	w.publishTaskQueue()
}

func taskResolved(d *sql.DB, id string) bool {
	if d == nil {
		return false
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM of_tasks WHERE id = ?`, id).Scan(&status); err != nil {
		return false
	}
	return status == TaskDone || status == TaskBlocked
}
