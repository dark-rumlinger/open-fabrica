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
// user_flows guide carry the how-to; this says what to do and that nothing else remains.
func flowGateInstruction() string {
	return "[mode: build]\n\n" +
		"The project is built and every fix testing found is done. One step remains: the user-flow tests.\n\n" +
		"Write of_test.js - the journeys a real person takes through THIS finished project - with " +
		"manage_testing action=write_flows, after recalling the user_flows guide's writing-flows section. " +
		"It replays as it saves, so a save already shows what is red. Fix what a red flow reveals, then replay " +
		"once with action=test_flows to check the fixes together - not after each single fix. " +
		"Keep it to at most 5 flows. For a red flow, follow the user_flows guide. Stop when the flows pass, or " +
		"after 3 replays in a row that get no closer to green; the post-build check replays once more and reports anything still red."
}

// flowPhaseSpec is the gate session's spec: the build system prompt and tools (over the
// ephemeral plan copy, so `when:flows` and the guide prefetch fire), but with the flow
// task message and a turn-end that lets the model stop on its own rather than on "every
// checklist item resolved" - the gate adds no checklist item.
func (w *Worker) flowPhaseSpec(plan *Plan) SessionSpec {
	spec := w.buildWholeSpec(plan, "")
	spec.MaxIterations = repairMaxIterations
	spec.OnTurnEnd = w.withOwnerInbox(func(int, []llm.ToolCall, int) (string, *ending) { return "", nil })
	spec.OnModelDone = func() string { return "" }
	seedFn := func() ([]llm.Message, error) {
		return seed{
			owner:  w.ownerContext() + w.ownerMemoryBlock(),
			shared: w.buildSharedSeed(plan),
			stable: w.buildIntentSeed(plan),
			task:   flowGateInstruction(),
		}.messages(), nil
	}
	spec.Seed, _ = seedFn()
	spec.Reseed = seedFn
	return spec
}

// runFlowGate runs the final user-flow phase and returns the flow findings to merge into
// the go-live sweep. A no-op (nil) when the owner turned user-flow tests off or the
// project has no pages or API to replay against.
func (w *Worker) runFlowGate(ctx context.Context, plan *Plan) ([]Issue, error) {
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
	w.publishMessage("Now writing and replaying the user-flow tests.")
	sess := w.newStageSession(StageBuild)
	res, err := w.runSpec(ctx, sess, w.flowPhaseSpec(planCopy))
	if w.run != nil {
		w.runlog.editStats(w.run.ID, flowGateRound, res)
	}
	if err == nil && res.Paused {
		if res.EndDetail != endOwnerStop.detail {
			w.pauseAwaitingAnswers()
		}
		return nil, errStagePaused
	}
	if err != nil {
		return nil, err
	}
	return w.runFlowValidator(ctx), nil
}

// runFlowValidator runs just the flow_tests validator once, for the authoritative
// go-live verdict after the gate session. Mirrors runValidators' context setup and
// panic handling for the one validator.
func (w *Worker) runFlowValidator(ctx context.Context) []Issue {
	baseURL, hostHeader := w.probeTarget()
	if baseURL == "" {
		return nil
	}
	ctx = w.validationContext(ctx)
	var fv Validator
	for _, v := range validators() {
		if v.Name() == flowTestsValidatorName {
			fv = v
			break
		}
	}
	if fv == nil {
		return nil
	}
	var issues []Issue
	func() {
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("validator panicked", "validator", fv.Name(), "panic", r)
				issues = []Issue{validatorCrashIssue(fv.Name())}
			}
		}()
		issues = fv.Validate(ctx, w.pdb.Reader(), baseURL, hostHeader)
	}()
	if len(issues) > 0 {
		w.logger.Info("validator found issues", "validator", fv.Name(), "count", len(issues))
		w.runlog.entry(w.run.ID, RunLogIssue, fv.Name(), map[string]any{"issues": issues}, 0, 0, 0)
	}
	return store.NormalizeSpeculative(issues)
}
