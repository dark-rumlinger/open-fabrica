package engine

// The BUILD stage builds the whole project in one continuous, prompt-cached session,
// then runs the validators once; what they find ships in the go-live report as known
// issues. The plan's items are a checklist the agent ticks off via manage_tasks, and the
// build stops when every item is done or blocked.
//
// One continuous conversation keeps the prompt cache warm (~0.1x reads). The seed is a
// pure function of DB state (intent + checklist), so rollover and crash resume both
// reseed deterministically.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"openfabrica/internal/flowhooks"
	"openfabrica/internal/flowsnap"
	"openfabrica/internal/msgcat"
	"openfabrica/probe"
	"openfabrica/providers/llm"
	"openfabrica/store"
)

type buildStage struct{}

func (buildStage) Name() string { return StageBuild }

func (buildStage) Info() StageInfo {
	return StageInfo{
		Timeout: 90 * time.Minute,
		StartLines: []string{
			msgcat.Mark("Building your project now, putting it together piece by piece."),
			msgcat.Mark("Time to build. Putting the pieces together now."),
			msgcat.Mark("Construction time. Assembling everything step by step."),
		},
	}
}

// validatorConcurrency bounds the parallel validator sweep. Matched to the project
// read pool's connection count: past that the extra goroutines only queue for a
// connection. Only the runtime probe needs the browser, and it manages its own tabs.
const validatorConcurrency = 4

func (buildStage) Run(ctx context.Context, w *Worker, r *Run) (Outcome, error) {
	// Repair bookkeeping belongs to one build: a dispute from an earlier run must not
	// hide a finding of this one, nor a stale signature attach to a reused item id.
	w.repairSigs, w.disputed, w.specAudits = nil, nil, 0
	plan := w.loadPlanOrEmpty(r)
	ownerAnswer := w.peekWakeAnswer()
	if ownerAnswer != "" {
		if n, err := ReopenBlockedTasks(w.pdb); err != nil {
			w.logger.Warn("build: reopening blocked items after the owner's answer failed", "error", err)
		} else if n > 0 {
			w.logger.Info("build: owner answered, reopened blocked checklist items", "count", n)
		}
	}
	w.publishTaskQueue()

	// Build the whole project in ONE continuous, prompt-cached session. On a
	// resume the checklist + project_state already reflect completed work, so
	// the agent continues from there and the done-checklist hook stops it fast.
	if _, err := w.runWholeBuild(ctx, plan, ownerAnswer); err != nil {
		return Outcome{}, err // errStagePaused propagates to drive() unchanged
	}

	// Test what was built: the validator sweep, which ends with the explore probe
	// clicking through every page. Fixable findings (and plan items the build left open) reopen the build for a
	// bounded repair round, after which everything is tested again. What is still wrong
	// after the last round ships as known issues. A user-flow finding is reported as a
	// flow that did not pass, not a defect.
	if ctx.Err() != nil {
		return Outcome{}, ctx.Err()
	}
	latest := w.runValidators(ctx)
	// The spec audit asks the other question: is what was built what was asked for.
	// Its gaps join the findings; after a repair round only the items it flagged are
	// audited again.
	gaps, _ := w.runSpecAudit(ctx, nil)
	for round := 1; round <= maxRepairRounds; round++ {
		before := append(append([]Issue(nil), latest...), gaps...)
		ran, err := w.repairRound(ctx, plan, repairFindings(before), round)
		if errors.Is(err, errStagePaused) {
			return Outcome{}, err
		}
		// Shutdown, the stage deadline and an exhausted budget end the run; they are not
		// a repair that failed, and shipping on them would mark an untested build live.
		if ctx.Err() != nil {
			return Outcome{}, ctx.Err()
		}
		if err != nil && isPermanentError(err) {
			return Outcome{}, err
		}
		if err != nil {
			// A failed repair never fails the build: what it did not fix ships flagged.
			w.logger.Warn("build: repair round failed, shipping what was built", "round", round, "error", err)
			break
		}
		if !ran {
			break
		}
		latest = w.dropDisputed(w.runValidators(ctx))
		if len(gaps) > 0 {
			// A re-audit that could not run keeps the known gaps: an unreadable reply
			// is not evidence that the repair closed them.
			if again, ok := w.runSpecAudit(ctx, gaps); ok {
				gaps = again
			}
		}
		// A round that left as many findings as it found is not worth another.
		if !repairProgress(before, append(append([]Issue(nil), latest...), gaps...)) && openPlanTasks(w.pdb.Reader()) == 0 {
			break
		}
	}
	// User-flow gate: now that the app is built and every fix testing found is done,
	// author and replay of_test.js against the FINISHED app (user-flow tests on, and the
	// project has pages or an API). It runs last, so the single "it's live" summary lands
	// after it, not before the QA that would otherwise contradict it.
	if flowIssues, ferr := w.runFlowGate(ctx, plan); ferr != nil {
		if errors.Is(ferr, errStagePaused) {
			return Outcome{}, ferr
		}
		if ctx.Err() != nil {
			return Outcome{}, ctx.Err()
		}
		if isPermanentError(ferr) {
			return Outcome{}, ferr
		}
		w.logger.Warn("build: flow gate failed, shipping without flow verification", "error", ferr)
	} else {
		latest = append(latest, flowIssues...)
	}
	w.dropOpenRepairTasks()
	// Gaps still open ship with the sweep's findings: as known issues for the owner,
	// in the run log for triage, and in the live agent's first wake.
	latest = append(latest, gaps...)
	// Drifting page styles are worth a repair attempt but are not a defect the owner can
	// hit, so what the repair rounds left of them ships as a warning, never as a failure.
	latest = styleDriftAsWarning(latest)
	knownIssues := false
	// issueReasons say, in owner-facing words, why the build ships flagged; the go-live
	// card lists them so "known issues" is never a bare warning.
	var issueReasons []string
	if fixable := FixableErrors(latest); len(fixable) > 0 && !onlyFlowIssues(fixable) {
		w.logger.Info("build: shipping with validator findings", "issues", len(fixable))
		knownIssues = true
		issueReasons = append(issueReasons, fmt.Sprintf("%d automatic check%s still failing after the build", len(fixable), pluralS(len(fixable))))
	}

	// Record the validator sweep as the authoritative shipped verdict, before the
	// go-live gate so it lands whether the build ships or is blocked.
	w.runlog.finalValidation(w.run.ID, latest)
	if crashed := countCategory(latest, validatorCrashCategory); crashed > 0 {
		knownIssues = true
		issueReasons = append(issueReasons, fmt.Sprintf("%d automatic check%s could not run, so part of the app is untested", crashed, pluralS(crashed)))
	}
	flowUnverified := unverifiedFlows(latest)
	flows := w.flowCheckFor(latest)

	// Unbuilt work is a known issue even when every validator is clean: validators only
	// inspect what was built, so a checklist item that never ran leaves nothing to find.
	// The checklist is the only record that the work was owed, so it decides this.
	unfinished, err := unfinishedTasks(w.pdb.Reader())
	if err != nil {
		// Report it: an unreadable checklist is the one case where this cannot tell
		// a finished build from an abandoned one, and silence there reads as success.
		w.logger.Warn("build: checklist unreadable, go-live report covers validators only", "error", err)
	}
	if len(unfinished) > 0 {
		w.logger.Info("build: shipping with unfinished checklist items", "count", len(unfinished))
		knownIssues = true
		issueReasons = append(issueReasons, fmt.Sprintf("%d planned task%s not finished", len(unfinished), pluralS(len(unfinished))))
	}

	// The go-live gate. Everything above may ship imperfect work (knownIssues); this
	// decides the one thing it must never cover: an app broken the moment someone opens
	// it. Reads the sweep above.
	if blocking := blockingErrors(latest); len(blocking) > 0 {
		switch policy := w.deployPolicy(); policy {
		case policyStrict:
			w.refuseGoLive(blocking)
			return Outcome{Done: true, Blocked: true}, nil
		default:
			// shadow: measure the blast radius without failing anyone yet. force: the owner
			// has already decided. Either way it ships, honestly labelled.
			w.logger.Warn("build: blocking issues did not stop the deploy",
				"policy", policy, "would_block", len(blocking),
				"categories", blockingCategorySummary(blocking))
			knownIssues = true
			issueReasons = append(issueReasons, fmt.Sprintf("%d serious problem%s may stop parts of the app from working", len(blocking), pluralS(len(blocking))))
		}
	}

	w.completeBuild(ctx, knownIssues, issueReasons, unfinished, flowUnverified, flows)

	return Outcome{Done: true}, nil
}

// runWholeBuild runs one continuous build session over the whole plan. A pending owner
// question pauses.
func (w *Worker) runWholeBuild(ctx context.Context, plan *Plan, ownerAnswer string) (SessionResult, error) {
	spec := w.buildWholeSpec(plan, ownerAnswer)
	sess := w.newStageSession(StageBuild)
	res, err := w.runSpec(ctx, sess, spec)
	if w.run != nil {
		w.runlog.editStats(w.run.ID, 0, res)
	}
	if err == nil && res.Paused {
		// An owner stop is parked by drive(), the single authority for stop
		// handling, so only a genuine owner question parks here. Parking a stop as
		// a question would label the run awaiting an answer that never comes, and
		// that label is what a resume reads to decide whether it may proceed. When
		// a stop raced a question, this still parks the question and drive's stop
		// branch preserves that reason over owner_stop.
		if res.EndDetail != endOwnerStop.detail {
			w.pauseAwaitingAnswers()
		}
		return res, errStagePaused
	}
	// Ending says how the session stopped, never whether the work got done: the
	// model can finish on its own terms with the plan half built. The checklist
	// answers that (see Run), which is why this only logs.
	w.logger.Info("build session ended",
		"ending", res.Ending, "detail", res.EndDetail, "tool_calls", res.ToolCalls)
	w.publishTaskQueue()
	return res, err
}

// buildWholeSpec assembles the SessionSpec for the whole build: one session over the
// entire project. The agent ticks checklist items via manage_tasks, and buildTurnEnd
// stops the session when every item is resolved. The seed builder is a pure function of
// DB state and doubles as the rollover Reseed, so a rollover/resume reseeds deterministically.
func (w *Worker) buildWholeSpec(plan *Plan, ownerAnswer string) SessionSpec {
	spec := DefaultSpec(StageBuild)
	spec.Guard = &RepeatGuard{}
	spec.PreloadTools = buildAllTools(plan)
	spec.System = w.buildSystemPrompt(plan)
	spec.OnTurnEnd = w.withOwnerInbox(w.buildTurnEnd())
	spec.OnModelDone = w.buildModelDone()
	spec.SeedGuides, _ = w.buildSeedGuides(plan)

	seedFn := func() ([]llm.Message, error) {
		return w.buildWholeSeed(plan, ownerAnswer), nil
	}
	spec.Seed, _ = seedFn()
	spec.Reseed = seedFn
	return spec
}

// withOwnerInbox hands owner messages that arrived mid-build to the session at its next
// turn boundary, appended after whatever the wrapped hook injects. A turn that stops the
// session leaves them queued: they would land after the last request, so the LIVE wake
// that follows the build answers them instead.
func (w *Worker) withOwnerInbox(next func(int, []llm.ToolCall, int) (string, *ending)) func(int, []llm.ToolCall, int) (string, *ending) {
	return func(iter int, calls []llm.ToolCall, writeRejects int) (string, *ending) {
		inject, stop := next(iter, calls, writeRejects)
		if stop != nil {
			return inject, stop
		}
		if note := ownerInboxNote(w.takeOwnerInbox(), w.replyLanguage()); note != "" {
			if inject != "" {
				inject += "\n\n"
			}
			inject += note
		}
		return inject, stop
	}
}

// batchCompleteNudgeAt is the manage_tasks completes-per-turn count at which a
// mid-build turn earns the one-time re-check nudge.
const batchCompleteNudgeAt = 4

// checklistCompleteStop is the stop message when every checklist item is resolved.
const checklistCompleteStop = "Every checklist item is resolved (done or blocked). Stop now - validation runs next."

// batchCompleteNudge is injected at most once per session when one mid-build turn
// completes many checklist items at once: batch closes are where items get the minimum.
const batchCompleteNudge = "You completed several checklist items in one turn. Completing in the same turn as the save is right; before moving on, re-check each of these against its done_when and the design intent, deepen any that fell short, then continue with the remaining items."

// finalBatchCompleteNudge is the same nudge for the turn that closes the LAST
// items. It needs its own wording because there are no remaining items to continue
// with, and this is the only turn left before the build ends.
const finalBatchCompleteNudge = "You just closed the last checklist items, several in one turn, so the build ends after this turn. Before it does: re-check each against its done_when and the design intent, and run the required end-of-build probe (manage_testing action=test_page on the real entry route, or test_module on each module when the project has no pages). Deepen anything thin, fix whatever the probe surfaces, then stop."

// maxRejectRepairTurns bounds how many extra turns a build may spend finishing edits
// that were rejected on the turn that would otherwise have ended it. Four: the model gets
// the rejection text back and a real fix usually lands on the first or second retry;
// past that the validators and the known-issues report own the problem.
const maxRejectRepairTurns = 4

// rejectedWriteRepair is injected when the closing turn had a rejected edit. A rejected
// write leaves the file HALF changed - the calls that succeeded landed, the rejected
// ones did not - so stopping there ships an import with nothing using it, or a caller
// with no callee: a build that ends on a turn whose import landed but whose body edit
// was refused goes live half-wired.
const rejectedWriteRepair = "Some edits in your last turn were REJECTED. A rejected patch changes nothing in its own file (its message says UNCHANGED), but other calls in the same turn may have landed, so a file can be left part-way: an import with nothing using it, or a caller with no callee. Read each rejection above, fix the cause, and re-apply the edit that failed. When one file needs several interdependent hunks (a new import PLUS the code that uses it), send them as ONE manage_files patch call with multiple entries in `patches`: split across separate calls, the earlier call is gated against a file the later call had not fixed yet, so it is refused and you are left with half the change."

// trackNudgeAfterSaves is how many file writes a build may make without completing a
// single checklist item before it earns the one-time Track reminder.
const trackNudgeAfterSaves = 6

// trackNudge is injected once when a build keeps writing files but has completed no
// checklist item: the owner's progress view sits at "not done" all build, then flips at
// once - the defect the build frame's Track rule names. Seen in a real run: a split game
// saved 23 modules across 17 turns and never called complete.
const trackNudge = "You have saved several files but completed no checklist item yet, so the owner's progress view still shows nothing done. Complete every item whose deliverables now exist and are wired (manage_tasks action=complete, one per task_id, with a one-line result), then keep to the Track step of your build frame."

// countFileWrites counts the manage_files save/patch actions in one turn's calls.
func countFileWrites(calls []llm.ToolCall) int {
	n := 0
	for _, c := range calls {
		if c.Name != toolFiles {
			continue
		}
		var a struct {
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(c.Arguments), &a) == nil && (a.Action == "save" || a.Action == "patch") {
			n++
		}
	}
	return n
}

// countTaskCompletes counts the manage_tasks complete actions in one turn's calls.
func countTaskCompletes(calls []llm.ToolCall) int {
	n := 0
	for _, c := range calls {
		if c.Name != toolTasks {
			continue
		}
		var a struct {
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(c.Arguments), &a) == nil && a.Action == "complete" {
			n++
		}
	}
	return n
}

// callsPageProbe reports whether the turn ran manage_testing's page or module probe.
func callsPageProbe(calls []llm.ToolCall) bool {
	for _, c := range calls {
		if c.Name != "manage_testing" {
			continue
		}
		var a struct {
			Action string `json:"action"`
		}
		if json.Unmarshal([]byte(c.Arguments), &a) == nil && (a.Action == "test_page" || a.Action == "test_module") {
			return true
		}
	}
	return false
}

// tasksHavePage reports whether any checklist item owns a page route or the shared layout.
func tasksHavePage(all []Task) bool {
	for _, t := range all {
		for _, p := range t.Paths {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "default" || isRootedNonAsset(p) {
				return true
			}
		}
	}
	return false
}

// finalProbeNudge asks for the end-of-build page probe the build frame requires when the
// session is about to stop without having run one.
const finalProbeNudge = "Every checklist item is resolved, but no page probe ran this build. Run `manage_testing action=test_page` on the real entry route now (a clean probe on the final wiring is the required end-of-build self-check), fix whatever it surfaces, then stop."

// buildTurnEnd drives the fresh build's stop machine without spending a model turn on
// the decision. A mid-build turn that batch-completes many items earns a one-time
// re-check nudge, and once every item is resolved (done or blocked) the session stops
// with checklist_complete. Publishes the queue each turn for the UI.
func (w *Worker) buildTurnEnd() func(int, []llm.ToolCall, int) (string, *ending) {
	// Seed the per-task completion tracker with items already done at session start, so a
	// resume never re-attributes a pre-existing completion. Only a transition to done
	// DURING this session emits a RunLogTask event, which buildTokenSummary windows build
	// LLM cost onto.
	prevDone := map[string]bool{}
	if all, err := LoadTasks(w.pdb.Reader()); err == nil {
		for _, t := range all {
			if t.Status == TaskDone {
				prevDone[t.ID] = true
			}
		}
	}
	repairTurns := 0
	// Track-rule reminder state: file writes and completes seen this session.
	sessionWrites, sessionCompletes := 0, 0
	trackNudged := false
	nudged := false
	probed, probeNudged := false, false
	return func(iter int, calls []llm.ToolCall, writeRejects int) (string, *ending) {
		w.publishTaskQueue()
		all, err := LoadTasks(w.pdb.Reader())
		if err != nil {
			// Transient DB trouble must not stop a build.
			return "", nil
		}
		if len(all) == 0 {
			// A zero-task checklist can never reach the resolved stop below, so
			// without this the session runs to its iteration cap.
			return checklistCompleteStop, &endChecklistComplete
		}
		// Scan every task (no early return): emit a completion event for each
		// item that newly reached done this turn, and decide the stop signal.
		allResolved := true
		for _, t := range all {
			if t.Status == TaskDone && !prevDone[t.ID] {
				prevDone[t.ID] = true
				w.runlog.taskComplete(w.run.ID, t.ID, t.Title, iter)
			}
			if t.Status != TaskDone && t.Status != TaskBlocked {
				allResolved = false
			}
		}
		// The batch check comes BEFORE the stop, and that ordering is the point.
		// Gating it behind !allResolved made it unreachable on the batch that needs
		// it most: a turn closing the LAST items ends the build immediately, so a
		// checklist rubber-stamped in one turn shipped with nothing re-examined. A model
		// can close every remaining item in a single turn and stop.
		// Costs at most one extra turn, once per session.
		// A turn whose edits were rejected has not done the work the checklist claims:
		// it left files half-changed. Finish the repair before any stop is considered.
		// The turn's probe, completions and writes count either way: skipping them on a
		// repair turn would ask again for a probe that already ran.
		if callsPageProbe(calls) {
			probed = true
		}
		completes := countTaskCompletes(calls)
		sessionWrites += countFileWrites(calls)
		sessionCompletes += completes
		if writeRejects > 0 && repairTurns < maxRejectRepairTurns {
			repairTurns++
			w.logger.Info("build: turn had rejected edits, asking for the repair before stopping",
				"rejects", writeRejects, "iteration", iter, "repair_turn", repairTurns)
			return rejectedWriteRepair, nil
		}
		if !trackNudged && !allResolved && sessionCompletes == 0 && sessionWrites >= trackNudgeAfterSaves {
			trackNudged = true
			w.logger.Info("build: files written but no checklist item completed, reminding", "writes", sessionWrites, "iteration", iter)
			return trackNudge, nil
		}
		if !nudged && completes >= batchCompleteNudgeAt {
			nudged = true
			if allResolved {
				return finalBatchCompleteNudge, nil
			}
			return batchCompleteNudge, nil
		}
		if !allResolved {
			return "", nil
		}
		if !probed && !probeNudged && tasksHavePage(all) {
			probeNudged = true
			w.logger.Info("build: no page probe ran, asking for one before stopping", "iteration", iter)
			return finalProbeNudge, nil
		}
		return checklistCompleteStop, &endChecklistComplete
	}
}

// openChecklistReprompt is injected when the model stops talking with checklist items
// still open. A model can narrate that everything is in place without calling complete,
// which ships a go-live report listing built work as undone.
const openChecklistReprompt = "You stopped, but these checklist items are still open:\n%s\nFor each one: if it is already built and wired, `manage_tasks action=complete` it now with its result. If it is not built, build it. If you need the owner's answer to go on, ask a chat question (`manage_chat action=ask`, load it with find_tool; it pauses the build); if you will ship without the item, `manage_tasks action=block` it with the reason. The owner is told what you leave open, so an item you built but never completed ships as work you did not do."

// buildModelDone reconciles the checklist when the model stops calling tools of its own
// accord. buildTurnEnd never sees that turn, so without this a build ends with items still
// pending and reports them to the owner as unbuilt. Fires at most once per session, and
// only for genuinely pending items: a blocked one is already resolved.
func (w *Worker) buildModelDone() func() string {
	asked := false
	return func() string {
		if asked {
			return ""
		}
		all, err := LoadTasks(w.pdb.Reader())
		if err != nil {
			// Same posture as buildTurnEnd: transient DB trouble must not hold a build open.
			return ""
		}
		var open []string
		for _, t := range all {
			if t.Status != TaskDone && t.Status != TaskBlocked {
				open = append(open, "- "+t.Title)
			}
		}
		if len(open) == 0 {
			return ""
		}
		asked = true
		w.logger.Info("build: model stopped with open checklist items, asking it to reconcile", "count", len(open))
		return fmt.Sprintf(openChecklistReprompt, strings.Join(open, "\n"))
	}
}

// loadPlanOrEmpty parses the run's plan, returning an empty Plan on any error
// so the loop still runs (the snapshot is ground truth; the plan supplies
// intent + guides). A stored plan that no longer parses is logged rather than
// dropped in silence: an empty plan on a resume loses the run's locked
// decisions and guides, and that is worth a line.
func (w *Worker) loadPlanOrEmpty(r *Run) *Plan {
	if r == nil || r.PlanJSON == "" {
		return &Plan{}
	}
	plan, err := ParsePlan(r.PlanJSON)
	if err == nil && plan != nil {
		return plan
	}
	if w != nil && w.logger != nil {
		w.logger.Warn("stored plan no longer parses, continuing without its decisions and guides", "error", err)
	}
	return &Plan{}
}

// validationContext carries what the post-build checks need past the read pool.
func (w *Worker) validationContext(ctx context.Context) context.Context {
	// Authenticate the runtime probes (no-op when the project has no auth) so
	// requireRole-gated pages execute their real data path instead of bouncing
	// to /login. The token rides the context; only the runtime probes read it.
	if tok := mintProbeAuthToken(w.pdb.Reader(), w.deps.JWTManager, w.projectID); tok != "" {
		ctx = probe.WithAuthToken(ctx, tok)
	}
	// Validators get the read pool; the flow validator also needs the writer to
	// delete the accounts its signup journeys register.
	ctx = probe.WithFlowWriteDB(ctx, w.pdb.Writer())
	// The flow validator and the explore probe write through the app (orders,
	// deletes, status changes) between a snapshot of its data and the restore.
	ctx = flowsnap.WithOpener(ctx, w.deps.DB.DB, w.pdb, w.deps.ProjectDBManager.ProjectDir(w.projectID), w.projectID)
	// Flows also reach past the page: the app's outgoing mail (kept, not sent),
	// seeded accounts, scheduled tasks and webhooks.
	return probe.WithFlowHooks(ctx, flowhooks.New(w.projectID, w.pdb.ToolHandle(ctx), w.deps.JWTManager, w.deps.Encryptor))
}

// validatorCrashCategory marks the warning a crashed validator leaves in its place, so a
// check that never ran is not read as a check that passed.
const validatorCrashCategory = "validator_crashed"

// countCategory counts the issues of one category.
func countCategory(issues []Issue, category string) int {
	n := 0
	for _, is := range issues {
		if is.Category == category {
			n++
		}
	}
	return n
}

func validatorCrashIssue(name string) Issue {
	return Issue{
		Severity: store.SeverityWarning,
		Category: validatorCrashCategory,
		Message:  "the `" + name + "` check crashed and did not run, so what it covers was not verified.",
	}
}

// runValidators executes every registered validator and pools their
// error-severity issues.
func (w *Worker) runValidators(ctx context.Context) []Issue {
	baseURL, hostHeader := w.probeTarget()
	if baseURL == "" {
		return nil
	}
	ctx = w.validationContext(ctx)
	// Validators are independent reads over the finished project, so they run
	// concurrently. Bounded to the project read pool's connection count, and results
	// are collected by index so the issue order stays deterministic: the fix loop
	// presents findings in this order, and a run that reordered them under scheduling
	// noise would be unreproducible.
	all := validators()
	// The flow validator runs only in the post-build flow gate (runFlowGate), never in
	// the in-loop sweep: during the build and its repair rounds of_test.js does not exist
	// yet, and the gate's own flow_tests run is the authoritative verdict.
	all = withoutFlowValidator(all)
	found := make([][]Issue, len(all))
	sem := make(chan struct{}, validatorConcurrency)
	var wg sync.WaitGroup
	for i, v := range all {
		if _, seq := v.(SequentialValidator); seq {
			continue // runs after the parallel batch, below
		}
		wg.Add(1)
		go func(i int, v Validator) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				// One validator panicking must not lose the whole sweep.
				if r := recover(); r != nil {
					w.logger.Error("validator panicked", "validator", v.Name(), "panic", r)
					found[i] = []Issue{validatorCrashIssue(v.Name())}
				}
			}()
			found[i] = v.Validate(ctx, w.pdb.Reader(), baseURL, hostHeader)
		}(i, v)
	}
	wg.Wait()
	for i, v := range all {
		if _, seq := v.(SequentialValidator); !seq {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					w.logger.Error("validator panicked", "validator", v.Name(), "panic", r)
					found[i] = []Issue{validatorCrashIssue(v.Name())}
				}
			}()
			found[i] = v.Validate(ctx, w.pdb.Reader(), baseURL, hostHeader)
		}()
	}

	var issues []Issue
	for i, v := range all {
		if len(found[i]) > 0 {
			w.logger.Info("validator found issues", "validator", v.Name(), "count", len(found[i]))
			w.runlog.entry(w.run.ID, RunLogIssue, v.Name(), map[string]any{"issues": found[i]}, 0, 0, 0)
		}
		issues = append(issues, found[i]...)
	}
	// Enforce the speculative invariant centrally: a structural inference with no
	// runtime witness is downgraded to a non-looping warning here, so it can never
	// drive a destructive fix-pass no matter which validator raised it.
	return store.NormalizeSpeculative(issues)
}
