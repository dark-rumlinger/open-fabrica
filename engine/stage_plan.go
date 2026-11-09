package engine

// The PLAN stage: one structured-output session turns the brief (or an owner
// change request) into a Plan - goal/voice/design plus the task queue - or
// into owner questions that pause the run.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"openfabrica/contracts"
	"openfabrica/events"
	"openfabrica/internal/msgcat"
	"openfabrica/internal/stringutil"
	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/store"
	"openfabrica/tools"
)

type planStage struct{}

// planAssembleInput is the ONE constructor of the PLAN prompt assembly, shared
// with the capability-visibility test so a surface the test checks (like
// HiddenGuides) cannot be silently dropped from the production path.
func planAssembleInput(ps []pack.Pack, reg *tools.Registry, caps llm.Capabilities, sentinels map[string]string) pack.AssembleInput {
	return pack.AssembleInput{
		Stage:     StagePlan,
		Wave:      pack.Wave,
		Selection: pack.Select(ps, StagePlan),
		Sentinels: sentinels,
		// Capability-dead guides are left out of the index PLAN picks from:
		// naming one lets the planner commit to a capability no tool can build.
		HiddenGuides: capabilityDeadGuides(ps, reg, caps),
	}
}

func (planStage) Name() string { return StagePlan }

func (planStage) Info() StageInfo {
	return StageInfo{
		Timeout: 30 * time.Minute,
		StartLines: []string{
			msgcat.Mark("Mapping it out. This is usually the longest part, so hang tight."),
			msgcat.Mark("Sketching the plan now. This stretch usually takes the longest."),
			msgcat.Mark("Working out the build plan. Give me a moment here."),
		},
	}
}

func (planStage) Run(ctx context.Context, w *Worker, r *Run) (Outcome, error) {
	// Crash recovery: pending questions mean we already asked - re-pause
	// without re-asking (unless this wake carries the answers).
	answers := w.peekWakeAnswer()
	if answers == "" && w.pendingQuestionCount() > 0 {
		w.pauseAwaitingAnswers()
		return Outcome{}, errStagePaused
	}

	ps := w.effectivePacks()
	caps, capsErr := llm.LoadCapabilities(w.deps.DB.DB)
	if capsErr != nil {
		w.logger.Warn("load capabilities failed; capability-gated tools hidden from plan catalogue", "error", capsErr)
	}

	// Resolve the task budget from the ambition level AND the brief (so an EXPAND-off
	// project whose brief specifies its scope is built in full, not capped). An update run
	// re-plans an existing project, so the budget is advisory there.
	budgetTarget, budgetCeiling, budgetEnforced := PlanTaskBudget(w.projectAmbitionLevel(), w.planBudgetBrief(r))
	isUpdateReplan := r != nil && r.UpdateDescription != ""
	if isUpdateReplan {
		budgetEnforced = false
	}

	// The capability guides a brief implies render at PLAN as the `## Decision:` blocks the
	// planner locks architecture decisions from (deeper sections a recall away). A small
	// structured LLM pre-pass (renderPlanGuides -> the picker in guide_prefetch.go) chooses
	// which guides this brief needs. Hoisted out of the Sentinels literal below because it is
	// a full LLM round-trip, easy to miss as an argument buried in a map literal.
	guidesCatalogue := w.renderPlanGuides(ctx, w.planGuideMatchText(r), w.loadPlanOrEmpty(r), withoutDeadGuides(ps, w.deps.ToolRegistry, caps))

	spec := DefaultSpec(StagePlan)
	// A plan nobody can parse is structurally unusable, so it must not stay in the replay
	// cache: the request behind it would only reproduce it. An over-budget or incoherent
	// plan is NOT this (it parses, it is a real answer) and the corrective retries handle it.
	spec.Accept = func(content string) error {
		_, err := parsePlanForStage(content)
		return err
	}
	spec.System = pack.Assemble(planAssembleInput(ps, w.deps.ToolRegistry, caps, map[string]string{
		"ALLOWED_COLUMN_TYPES": "`" + strings.Join(allowedColumnTypes, "` | `") + "`",
		"GUIDES_CATALOGUE":     guidesCatalogue,
		// The build-stage tool catalogue renders ONCE here (per build) so the planner
		// can declare each task's tool needs; it no longer rides every build call's prompt.
		"TOOL_CATALOGUE": w.deps.ToolRegistry.Index(caps, StageBuild),
		// The task budget: carries the ambition dial (and, for EXPAND-off, the brief's
		// own detail) into the plan, so a brief that overflows one build session is
		// planned as a coherent core with the rest in `exclusions`.
		"TASK_BUDGET": planTaskBudgetDirective(w.projectAmbitionLevel(), budgetTarget, budgetCeiling, budgetEnforced, isUpdateReplan),
		// EXPAND is the single home for stack-correction, but is SKIPPED for
		// ambition=off, update runs, and empty briefs, where PLAN reads the RAW brief.
		// STACK_NOTE tells the planner which case it is in, so the frame never asserts a
		// correction that did not run. See planStackNote.
		"STACK_NOTE": planStackNote(r),
	})).System
	spec.ResponseSchema = PlanResponseSchema()

	// The brief's features: the list the plan must cover. A fresh plan extracts them
	// from the brief it reads; an update re-plan keeps the ones the project was built to.
	var features []PlanFeature
	if isUpdateReplan {
		features = w.loadPlanOrEmpty(r).Features
	} else {
		features = w.extractFeatures(ctx, planBriefText(w.projectBrief(), r))
	}

	// The ask, built once. Each corrective turn below re-asks it with the previous answer
	// and a critique appended, so they share this base rather than restating it.
	base := seed{
		owner: w.ownerContext() + w.ownerMemoryBlock(),
		task:  buildPlanUserMessage(w.projectBrief(), answers, r, features),
	}
	spec.Seed = base.messages()

	sess := w.newStageSession(StagePlan)

	// Structured output is required: fail loudly when the provider can't.
	rp, err := sess.resolver.resolve()
	if err != nil {
		return Outcome{}, err
	}
	if !supportsStructuredOutput(rp) {
		return Outcome{}, SchemaUnsupportedError(rp.Provider.Type(), rp.Model)
	}

	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		return Outcome{}, err
	}

	plan, perr := parsePlanForStage(res.Content)
	if perr != nil {
		// One corrective retry with the parse error fed back verbatim, almost always model
		// noise that resolves when told what was wrong. Log the RAW output too: ParsePlan
		// repairs and discards it internally, so this is the only record of the failed bytes.
		w.logger.Warn("plan parse failed, retrying once with corrective feedback",
			"error", perr, "raw_response", stringutil.Truncate(res.Content, planRawLogLimit))
		spec.Seed = base.answered(res.Content, fmt.Sprintf(
			"Your previous JSON could not be parsed:\n\n%s\n\nRe-emit the FULL plan JSON, fixing the issue above. Same shape as before, keep every other field intact. Output only the JSON object, no prose, no markdown fences.",
			perr.Error())).messages()
		res, err = w.runSpec(ctx, sess, spec)
		if err != nil {
			return Outcome{}, err
		}
		plan, perr = parsePlanForStage(res.Content)
		if perr != nil {
			w.logger.Warn("plan parse failed after corrective retry",
				"error", perr, "raw_response", stringutil.Truncate(res.Content, planRawLogLimit))
			return Outcome{}, fmt.Errorf("plan JSON invalid (after retry): %w", perr)
		}
	}

	// Owner-question handoff: persist questions and pause.
	if len(plan.Questions) > 0 {
		if len(plan.Questions) > MaxPlanQuestions {
			plan.Questions = plan.Questions[:MaxPlanQuestions]
		}
		w.insertPlanQuestions(plan.Questions)
		w.pauseAwaitingAnswers()
		return Outcome{}, errStagePaused
	}

	// Enforce the task ceiling, but only when enforced (an owner-specified detailed brief
	// with EXPAND off is built as written). A plan over budget grinds the build toward the
	// context/time wall, so push back ONCE to keep the coherent core and defer the rest to
	// `exclusions`. Still over afterwards, we proceed (logged): a large build beats nothing.
	if budgetEnforced && len(plan.Tasks) > budgetCeiling {
		ceiling := budgetCeiling
		w.logger.Info("plan over task budget, retrying once to trim",
			"tasks", len(plan.Tasks), "ceiling", ceiling, "ambition", w.projectAmbitionLevel())
		if plan, err = w.rerunPlanWithCritique(ctx, sess, &spec, base,
			res.Content, planOverBudgetMessage(len(plan.Tasks), ceiling), "plan budget-retry", plan); err != nil {
			return Outcome{}, err
		}
		if len(plan.Tasks) > ceiling {
			w.logger.Info("plan still over task budget after retry, proceeding",
				"tasks", len(plan.Tasks), "ceiling", ceiling)
		}
	}

	// The floor is now prompt-only: the enforced high-ambition directive leads with
	// the target as a firm minimum (see planTaskBudgetDirective), but an undershot
	// plan is no longer bounced for a coverage retry - it seeds as written.

	// Plan-time coherence: catch internal contradictions the prompt forbids but nothing
	// enforced (see planCoherenceFeedback). Like the budget/parse retries, ONE bounded
	// corrective turn; still incoherent afterwards we proceed (logged). Update runs check
	// the auth surface against the LIVE project, since a full re-plan legitimately omits an
	// auth surface the project already has.
	projectHasAuth := false
	if r != nil && r.UpdateDescription != "" && w.pdb != nil {
		projectHasAuth = store.ProjectHasAuth(w.pdb.Reader())
	}
	// Deterministic repairs first: a mechanical, unambiguous fix (e.g. homing entry
	// exports in a store module) is applied engine-side rather than spent as an LLM
	// re-plan turn (see plan_normalize.go). Only what survives goes to the retry.
	if fixes := normalizeEntryExports(plan); len(fixes) > 0 {
		w.logger.Info("plan normalized deterministically", "fixes", strings.Join(fixes, "; "))
	}
	if fb := joinFeedback(planCoherenceFeedback(plan, projectHasAuth), featureCoverageFeedback(plan, features)); fb != "" {
		w.logger.Info("plan has coherence issues, retrying once to repair", "detail", stringutil.Truncate(fb, 500))
		// Feeds back the re-marshalled canonical plan, not the raw response: by here the
		// plan has been parsed and maybe replaced by a retry, so the raw bytes are stale.
		curJSON, _ := json.Marshal(plan)
		if plan, err = w.rerunPlanWithCritique(ctx, sess, &spec, base,
			string(curJSON), fb, "plan coherence-retry", plan); err != nil {
			return Outcome{}, err
		}
		// Re-normalize the repaired plan so a re-slipped export home is fixed
		// deterministically instead of only logged as still-incoherent.
		normalizeEntryExports(plan)
		if still := joinFeedback(planCoherenceFeedback(plan, projectHasAuth), featureCoverageFeedback(plan, features)); still != "" {
			w.logger.Info("plan still has coherence issues after retry, proceeding", "detail", stringutil.Truncate(still, 500))
		}
	}
	// A feature no task builds is never built and no validator can see the gap, so it gets
	// one more bounded retry of its own: a planner that fixed the contradictions but left a
	// feature uncovered usually covers it when only that is asked.
	if fb := featureCoverageFeedback(plan, features); fb != "" {
		w.logger.Info("plan leaves brief features uncovered, retrying once more", "detail", stringutil.Truncate(fb, 500))
		curJSON, _ := json.Marshal(plan)
		if next, rerr := w.rerunPlanWithCritique(ctx, sess, &spec, base, string(curJSON), fb, "plan feature-retry", plan); rerr != nil {
			return Outcome{}, rerr
		} else {
			plan = next
		}
		normalizeEntryExports(plan)
		if still := featureCoverageFeedback(plan, features); still != "" {
			w.logger.Info("plan still leaves features uncovered, proceeding", "detail", stringutil.Truncate(still, 500))
		}
	}

	// User flows are not a planning-time checklist item: they run as a final gate after
	// the build and repair settle (runFlowGate). Strip any flow item the planner invented
	// so it does not sit on the checklist with nothing to work it, whether tests are on or off.
	if dropUserFlowsTasks(plan) {
		w.logger.Info("plan: dropped a planner-invented user-flows checklist item (flows run as a post-build gate)", "tasks", len(plan.Tasks))
	}

	// Tasks that own the same file merge into one before the plan is stored, so the stored
	// plan, the feature coverage the audit reads and the queue all name the same tasks.
	plan.Tasks = mergeOverlappingPlanTasks(w, plan.Tasks)

	// The planner never writes the feature list; the engine attaches the one it checked
	// the plan against, for BUILD and the spec audit to read.
	plan.Features = features
	planJSON, _ := json.Marshal(plan)
	if err := r.setPlan(w.pdb, string(planJSON)); err != nil {
		w.logger.Warn("persist plan failed", "error", err)
	}

	// Surface an oversized single-session build in the run log (advisory only -
	// never blocks; the planner legitimately scales tasks with project size).
	if adv := PlanScopeAdvisory(plan); adv != "" {
		w.logger.Info("plan scope advisory", "detail", adv, "tasks", len(plan.Tasks))
	}

	// A decision check the conformance validator cannot evaluate (unknown op, or an
	// artifact/field outside its vocabulary) is skipped-as-pass by design; log each disabled
	// check so it doesn't vanish silently.
	for _, warn := range planCheckWarnings(plan) {
		w.logger.Warn("plan decision check disabled", "detail", warn)
	}

	// Reseed the queue: a fresh plan replaces any prior queue.
	// A queue that did not clear, or seeded only part of the plan, would build a project
	// that silently lacks the missing items (or none: an empty queue ends BUILD at once),
	// so either is an error the drive loop retries.
	if err := ClearTasks(w.pdb); err != nil {
		return Outcome{}, fmt.Errorf("clearing the task queue: %w", err)
	}
	seeded := seedTasksFromPlan(w, r, plan)
	if seeded == 0 || seeded < len(plan.Tasks) {
		_ = ClearTasks(w.pdb)
		return Outcome{}, fmt.Errorf("seeding the checklist stored %d of %d plan tasks", seeded, len(plan.Tasks))
	}

	if intro := strings.TrimSpace(plan.Intro); intro != "" {
		w.publishVerbatim(intro)
	}
	w.publishMessage(fmt.Sprintf(pickPhrase(
		"Here's my plan: **%d things** to build for you. Starting now.",
		"Plan's ready: **%d things** to build. Starting now.",
		"Mapped out **%d things** to build. Let's get going.",
	), seeded))

	return Outcome{Advance: true}, nil
}

// rerunPlanWithCritique feeds prevContent plus feedback back for one corrective
// turn and returns the re-parsed plan when it parses AND still carries tasks. A
// retry that errors, fails to parse, or comes back empty leaves fallback in
// place. label names the retry in logs. Shared by the budget and coherence
// retries, which differ only in what they feed back and how they critique.
func (w *Worker) rerunPlanWithCritique(ctx context.Context, sess *session, spec *SessionSpec, base seed, prevContent, feedback, label string, fallback *Plan) (*Plan, error) {
	spec.Seed = base.answered(prevContent, feedback).messages()
	res, err := w.runSpec(ctx, sess, *spec)
	if err != nil {
		return fallback, err
	}
	if adopted, perr := parsePlanForStage(res.Content); perr == nil && len(adopted.Tasks) > 0 {
		return adopted, nil
	} else if perr != nil {
		w.logger.Warn(label+" parse failed, keeping pre-retry plan", "error", perr)
	}
	return fallback, nil
}

// planTaskBudgetDirective renders the {{TASK_BUDGET}} sizing bullet. Enforced: it states
// the target, the hard ceiling, and the core-first / exclusions rule. Advisory: build the
// full scope and treat the size as rough guidance, so requested work is never deferred to
// hit a number (an update run re-plans an existing project; an EXPAND-off brief lays out
// its own scope).
func planTaskBudgetDirective(level string, target, ceiling int, enforced, isUpdateReplan bool) string {
	if !enforced {
		if isUpdateReplan {
			return fmt.Sprintf(
				"- **Task budget: build the full scope this request needs (~%d tasks as a rough sense of size, NOT a ceiling).** This is a re-plan of an existing project, so the size is advisory - write a task for every capability the request needs and do NOT defer requested work to `exclusions` to hit a number. Group the work into cohesive tasks (foundational data/shell tasks count too).",
				target)
		}
		return fmt.Sprintf(
			"- **Task budget: build the full scope this brief specifies (~%d tasks as a rough sense of size, NOT a ceiling).** Auto-expansion is off and the brief already lays out its scope, so that scope is the owner's deliberate choice - write a task for every capability it names and do NOT defer owner-specified work to `exclusions` to hit a number. Just group the work into cohesive tasks (foundational data/shell tasks count too); if the brief genuinely implies many tasks, that is fine.",
			target)
	}
	// The high tier (AmbitionTaskFloor > 0) leads with the target as a firm minimum,
	// not a soft "fill about N": the expansive tier exists to be filled, and plans
	// tend to undershoot its target when the ceiling leads. This is prompt-only
	// pressure now - an undershot plan is no longer bounced for a coverage retry, so the
	// wording carries the whole weight. The floor number itself is never stated (it only
	// invited anchoring); the target gets the teeth.
	if AmbitionTaskFloor(level) > 0 {
		return fmt.Sprintf(
			"- **Task budget (ambition: %s): build about %d cohesive build tasks and treat that as the size to reach, not a stretch goal (foundational data/shell tasks count too).** A plan that lands well short of it ships a thinner product than the owner opted into, so cover the full scope on the first pass: write a task for every WHOLE capability the brief implies (pull anything you were about to defer back out of `exclusions`), and split any mega-task into cohesive subsystem leaves. Never pad to reach the number: no filler, no stubs, no verification/testing/QA/polish tasks (the platform validates the finished build itself), no shattering one page into fragments. The hard ceiling is %d: only if the brief genuinely overflows one build session, %s A plan over that ceiling is sent back once to trim, so size it right the first time.",
			NormalizeAmbition(level), target, ceiling, planCoreCutClause)
	}
	// Lower tiers have no under-coverage gate, so a lean plan is fine: the target is a rough
	// size and only the ceiling is enforced.
	return fmt.Sprintf(
		"- **Task budget (ambition: %s): fill about %d build tasks, hard ceiling %d (foundational data/shell tasks count too).** A single continuous build session can only finish so much before context accumulation and the time limit make it grind, so the whole plan must fit this budget. If the brief describes more than fits, %s A plan over the ceiling is sent back to you once to trim, so size it right the first time.",
		NormalizeAmbition(level), target, ceiling, planCoreCutClause)
}

// planCoreCutClause is how a plan is trimmed to fit: keep a coherent vertical
// slice, defer whole capabilities rather than thinning every one. One constant
// because the rule is stated twice in the same run (budget directive and
// over-budget retry) and the two copies must not drift apart.
const planCoreCutClause = "build the COHERENT CORE - the foundational data/shell tasks " +
	"plus the most essential capabilities that together make a complete, shippable vertical " +
	"slice - and move everything you defer into the `exclusions` array with a one-line reason " +
	"each. Cut by dropping WHOLE capabilities, never by thinning them into shallow stubs: " +
	"fewer complete features beat many half-built ones."

// planBudgetBrief returns the text the budget classifier reads: the expanded brief when
// EXPAND ran, else the raw owner brief. Mirrors the brief buildPlanUserMessage plans from.
func (w *Worker) planBudgetBrief(r *Run) string {
	if r != nil && r.ExpandedBrief != "" {
		return r.ExpandedBrief
	}
	return w.projectBrief()
}

// planGuideMatchText is the text guide selection matches against: the raw brief, the
// expanded brief, and any update description concatenated, for high recall on both fresh
// builds and change requests (an update names an added capability in the update
// description; the original scope stays via the persisted brief).
func (w *Worker) planGuideMatchText(r *Run) string {
	parts := []string{w.projectBrief()}
	if r != nil {
		parts = append(parts, r.ExpandedBrief, r.UpdateDescription)
	}
	return strings.Join(parts, "\n")
}

// planOverBudgetMessage is the corrective-retry prompt when a plan exceeds the
// task ceiling: re-emit cut to the core, the rest deferred to exclusions.
func planOverBudgetMessage(have, ceiling int) string {
	return fmt.Sprintf(
		"Your plan has %d build tasks, over the hard ceiling of %d for this project's ambition level. A single continuous build session cannot finish that many before it grinds against the context and time limits. Re-emit the FULL plan JSON cut to AT MOST %d tasks: %s Do not wire a nav tab or link to anything you moved to exclusions (no dead links). Keep every other field intact. Output only the JSON object, no prose, no markdown fences.",
		have, ceiling, ceiling, planCoreCutClause)
}

// planStackNote renders the {{STACK_NOTE}} sentinel. EXPAND owns stack-correction but is
// skipped for ambition=off, update runs, and empty briefs, so this tells the planner
// whether to trust an already-corrected brief or self-correct.
func planStackNote(r *Run) string {
	if r != nil && r.ExpandedBrief != "" {
		return "The brief below has already been stack-corrected by EXPAND (any non-browser " +
			"tech was rewritten to a browser-native equivalent); trust it."
	}
	return "The brief below is the owner's raw text - EXPAND did not run, so YOU must " +
		"stack-correct it: silently drop anything that cannot run as browser HTML/CSS/JS over " +
		"an SQLite-backed Go API (npm/bundler/SSR steps, a non-Go backend, a non-SQLite " +
		"database, a native/mobile runtime) and plan the browser-native equivalent instead. " +
		"CDN-served UMD/ESM libraries (Tailwind, three.js, Phaser, GSAP, ...) are fine as-is."
}

// buildPlanUserMessage assembles the PLAN user prompt from the brief, prior
// owner answers, and any in-flight change request (update runs re-plan
// against the current plan).
func buildPlanUserMessage(brief, answers string, r *Run, features []PlanFeature) string {
	var b strings.Builder
	if r != nil && r.UpdateDescription != "" && r.PlanJSON != "" {
		b.WriteString("The owner is requesting a change to an existing project:\n")
		b.WriteString(r.UpdateDescription)
		b.WriteString("\n\nCurrent plan:\n```json\n")
		b.WriteString(r.PlanJSON)
		b.WriteString("\n```\n\n")
		// An update run can pause on questions like a fresh one, so the answers must
		// reach the re-run here too; without them the planner re-asks or plans blind.
		writePlanAnswers(&b, answers)
		if stub := planSecurityContract(r.UpdateDescription); stub != "" {
			b.WriteString(stub)
			b.WriteString("\n\n")
		}
		if fs := renderFeaturesForPlan(features); fs != "" {
			b.WriteString(fs + "\n")
		}
		b.WriteString("Produce the updated full plan JSON now.")
		return b.String()
	}
	effectiveBrief := planBriefText(brief, r)
	b.WriteString("Project brief:\n")
	b.WriteString(effectiveBrief)
	b.WriteString("\n\n")
	// An update run with no stored plan to revise still carries the owner's change.
	if r != nil && r.UpdateDescription != "" {
		b.WriteString("The owner is requesting this change:\n")
		b.WriteString(r.UpdateDescription)
		b.WriteString("\n\n")
	}
	writePlanAnswers(&b, answers)
	if stub := planSecurityContract(effectiveBrief); stub != "" {
		b.WriteString(stub)
		b.WriteString("\n\n")
	}
	if fs := renderFeaturesForPlan(features); fs != "" {
		b.WriteString(fs + "\n")
	}
	b.WriteString("Produce the plan JSON now.")
	return b.String()
}

// writePlanAnswers appends the owner's answers to the planner's earlier questions.
// Shared by the fresh and update branches of buildPlanUserMessage.
func writePlanAnswers(b *strings.Builder, answers string) {
	if answers == "" {
		return
	}
	b.WriteString("Owner's answers to your prior questions:\n")
	b.WriteString(answers)
	b.WriteString("\n\n")
}

// planRawLogLimit caps the raw model output logged when a plan fails to parse. It sits
// above a full-size plan on purpose: the defect is as likely in the tail as the head, so
// a tighter clip could hide it. Only a failing plan is ever logged at this size.
const planRawLogLimit = 32000

// parsePlanForStage decodes PLAN output and rejects an unactionable plan (no
// tasks AND no questions) so a misspelled "tasks" key routes into the
// corrective-retry path instead of shipping an empty build.
func parsePlanForStage(raw string) (*Plan, error) {
	p, err := ParsePlan(raw)
	if err != nil {
		return nil, err
	}
	if len(p.Tasks) == 0 && len(p.Questions) == 0 {
		return nil, fmt.Errorf(
			"plan has no build tasks: the \"tasks\" array is empty (it may have been " +
				"emitted under a misspelled key). Every plan must contain at least one " +
				"task with title, intent and done_when, unless you are asking the owner a question")
	}
	return p, nil
}

// planSecurityContract inlines a condensed E2EE security model into the PLAN message when
// the brief implies a private/encrypted realtime app, so the planner doesn't invent the
// password-column anti-pattern the build-time guide then undoes. Renders the single-source
// `e2ee-security-model` contract, which reaches a prompt only here.
func planSecurityContract(brief string) string {
	if !briefImpliesEncryption(brief) {
		return ""
	}
	return contracts.MustRender(contracts.IDE2EESecurityModel)
}

// briefImpliesEncryption is a high-precision check for encrypted-app briefs. Explicit
// encryption wording always qualifies. The password-co-occurrence inference is VETOED by
// account-shaped signals: the E2EE model REPLACES server-side auth, so appending it to a
// brief that demands accounts (register/login, roles) would plant a direct contradiction.
func briefImpliesEncryption(brief string) bool {
	s := strings.ToLower(brief)
	for _, kw := range []string{"encrypt", "end-to-end", "end to end", "e2ee", "e2e "} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	for _, kw := range []string{"register", "sign up", "signup", "account", "admin", "log in", "login", "role"} {
		if strings.Contains(s, kw) {
			return false
		}
	}
	// Content-secrecy intent stated without the literal "encrypt" - messages only the
	// participants can read, or that self-destruct. AFTER the account veto: E2EE
	// replaces server-side auth, so a brief that also wants accounts is the exact
	// contradiction the veto rejects.
	for _, kw := range []string{"nobody else can read", "no one else can read", "only participants", "only the participants", "disappearing message", "self-destruct"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	if strings.Contains(s, "password") {
		for _, ctx := range []string{"room", "call", "video", "voice", "chat", "join", "peer"} {
			if strings.Contains(s, ctx) {
				return true
			}
		}
	}
	return false
}

// seedTasksFromPlan converts the planner's tasks into checklist rows. Declared dep ids are
// slugified to match assigned ids; a dep that resolves to no seeded task is logged and
// dropped so the checklist never carries a dangling reference.
func seedTasksFromPlan(w *Worker, r *Run, plan *Plan) int {
	if plan == nil || len(plan.Tasks) == 0 {
		return 0
	}
	// Enforce the "no two tasks own the same file" invariant the planner is told to honour
	// but cannot guarantee. Overlapping owned paths otherwise run as separate sessions that
	// redo each other's work and force cross-task re-reads.
	planTasks := mergeOverlappingPlanTasks(w, plan.Tasks)

	// Assign each task its FINAL queue id up front, mirroring AddTasks' uniqueSlug (explicit
	// id, else a title slug, suffixed on collision). Dep resolution runs against these, not
	// just explicit ids: the planner MAY omit `id`, so indexing by explicit id alone drops
	// deps that reference an id-less task by its natural slug (losing page-before-table order).
	assigned := make([]string, len(planTasks))
	taken := make(map[string]bool, len(planTasks))
	known := make(map[string]bool, len(planTasks))
	for i, t := range planTasks {
		id := stringutil.UniqueTaskSlug(t.ID, t.Title, taken)
		taken[id] = true
		assigned[i] = id
		known[id] = true
	}

	tasks := make([]Task, 0, len(planTasks))
	for i, t := range planTasks {
		deps := make([]string, 0, len(t.Deps))
		seen := make(map[string]bool, len(t.Deps))
		for _, d := range t.Deps {
			resolved, ok := resolvePlanDep(d, assigned, known)
			if !ok {
				w.logger.Warn("plan: task declares an unknown dependency, dropping it",
					"task", assigned[i], "title", t.Title, "dep", d)
				continue
			}
			if resolved == assigned[i] {
				w.logger.Warn("plan: task depends on itself, dropping it",
					"task", assigned[i], "title", t.Title, "dep", d)
				continue
			}
			if seen[resolved] {
				continue
			}
			seen[resolved] = true
			deps = append(deps, resolved)
		}
		tasks = append(tasks, Task{
			ID:       assigned[i],
			Title:    t.Title,
			Intent:   t.Intent,
			DoneWhen: t.DoneWhen,
			Deps:     deps,
			Paths:    t.Paths,
			Edits:    t.Edits,
			Tools:    t.Tools,
			Exports:  t.Exports,
			Origin:   TaskOriginPlan,
			Status:   TaskPending,
		})
	}
	ids, err := AddTasks(w.pdb, r.ID, tasks)
	if err != nil {
		w.logger.Warn("seed tasks failed", "error", err)
	}
	return len(ids)
}

// normPlanPath normalises a planner-declared path for overlap comparison (case, leading
// slash, /files/ prefix) so "/files/list.js", "files/list.js", and "list.js" compare equal.
// A page route and a resource/table name are DIFFERENT namespaces ("/activity" the page vs
// "activity" the table), and folding them would merge a page task into a backend one and
// drop the page. So a root-absolute non-/files/ path keeps its slash; only file assets fold.
func normPlanPath(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	// The shared layout's canonical path is `default`; the plan frame tells the planner
	// `/layout` normalizes to it, so it must never read as a page route here (the
	// app-shape route-sprawl check would count it).
	if p == "/layout" {
		return "default"
	}
	if isRootedNonAsset(p) {
		return p // page-route namespace: keep the slash so it can't alias a bare name
	}
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimPrefix(p, "files/")
	return p
}

// resolvePlanDep maps a planner-declared dependency string onto the id of the
// task it means: an exact match (the dep slugifies to an assigned id), else a
// unique token-boundary prefix of exactly one assigned id - a prefix matching
// two or more ids is ambiguous and left unresolved. Returns the resolved id and
// true, or "" and false when nothing matches.
func resolvePlanDep(dep string, assigned []string, known map[string]bool) (string, bool) {
	s := stringutil.TaskSlug(dep)
	if s == "" {
		return "", false
	}
	if known[s] {
		return s, true
	}
	match, n := "", 0
	for _, a := range assigned {
		if strings.HasPrefix(a, s+"-") {
			match = a
			n++
		}
	}
	if n == 1 {
		return match, true
	}
	return "", false
}

// planTaskSlug is the queue id a PlanTaskSpec resolves to (explicit id, else
// derived from the title) - the same mapping seedTasksFromPlan/AddTasks use.
func planTaskSlug(t PlanTaskSpec) string {
	if s := stringutil.TaskSlug(t.ID); s != "" {
		return s
	}
	return stringutil.TaskSlug(t.Title)
}

// mergeOverlappingPlanTasks folds together tasks that list the SAME path in their `paths`
// (both claim ownership), the invariant the plan states but a model can violate. It merges
// the later task into the earlier (unioning fields, re-pointing dependents), skipping any
// merge that would cycle. Edits never trigger a merge (the intended cross-module pattern).
// Pure over the ordered slice, so a re-plan re-derives the same queue.
func mergeOverlappingPlanTasks(w *Worker, in []PlanTaskSpec) []PlanTaskSpec {
	if len(in) < 2 {
		return in
	}
	specs := append([]PlanTaskSpec(nil), in...)
	ids := make([]string, len(specs))
	for i := range specs {
		ids[i] = planTaskSlug(specs[i])
	}
	removed := make([]bool, len(specs))

	pathsOverlap := func(a, b PlanTaskSpec) bool {
		set := map[string]bool{}
		for _, p := range a.Paths {
			if n := normPlanPath(p); n != "" {
				set[n] = true
			}
		}
		for _, p := range b.Paths {
			if n := normPlanPath(p); n != "" && set[n] {
				return true
			}
		}
		return false
	}

	// wouldCycle reports whether re-pointing `from`'s id onto `into` (the
	// pending merge) leaves a dependency cycle among the live tasks.
	wouldCycle := func(fromIdx, intoIdx int) bool {
		remap := func(slug string) string {
			if slug == ids[fromIdx] {
				return ids[intoIdx]
			}
			return slug
		}
		adj := map[string][]string{}
		for i := range specs {
			if removed[i] || i == fromIdx {
				continue
			}
			fromID := remap(ids[i])
			deps := specs[i].Deps
			if i == intoIdx {
				deps = append(append([]string{}, specs[intoIdx].Deps...), specs[fromIdx].Deps...)
			}
			for _, d := range deps {
				to := remap(stringutil.TaskSlug(d))
				if to == "" || to == fromID {
					continue
				}
				adj[fromID] = append(adj[fromID], to)
			}
		}
		const white, gray, black = 0, 1, 2
		color := map[string]int{}
		var dfs func(string) bool
		dfs = func(u string) bool {
			color[u] = gray
			for _, v := range adj[u] {
				if color[v] == gray {
					return true
				}
				if color[v] == white && dfs(v) {
					return true
				}
			}
			color[u] = black
			return false
		}
		for i := range specs {
			if removed[i] || i == fromIdx {
				continue
			}
			u := remap(ids[i])
			if color[u] == white && dfs(u) {
				return true
			}
		}
		return false
	}

	for i := range specs {
		if removed[i] {
			continue
		}
		for j := i + 1; j < len(specs); j++ {
			if removed[j] || ids[j] == "" || !pathsOverlap(specs[i], specs[j]) {
				continue
			}
			if wouldCycle(j, i) {
				w.logger.Warn("plan: tasks share an owned path but merging would cycle; leaving them split",
					"keep", ids[i], "other", ids[j])
				continue
			}
			specs[i].Paths = unionStrings(specs[i].Paths, specs[j].Paths)
			specs[i].Edits = unionStrings(specs[i].Edits, specs[j].Edits)
			specs[i].Deps = unionStrings(specs[i].Deps, specs[j].Deps)
			specs[i].Tools = unionStrings(specs[i].Tools, specs[j].Tools)
			specs[i].Exports = unionStrings(specs[i].Exports, specs[j].Exports)
			specs[i].Covers = unionStrings(specs[i].Covers, specs[j].Covers)
			if s := strings.TrimSpace(specs[j].Intent); s != "" {
				specs[i].Intent = strings.TrimSpace(specs[i].Intent + "\n" + s)
			}
			if s := strings.TrimSpace(specs[j].DoneWhen); s != "" {
				specs[i].DoneWhen = strings.TrimSpace(specs[i].DoneWhen + " " + s)
			}
			for k := range specs {
				if removed[k] {
					continue
				}
				specs[k].Deps = remapDep(specs[k].Deps, ids[j], ids[i], ids[k])
			}
			removed[j] = true
			w.logger.Info("plan: merged tasks that own the same file", "into", ids[i], "merged", ids[j])
		}
	}

	out := make([]PlanTaskSpec, 0, len(specs))
	for i := range specs {
		if !removed[i] {
			out = append(out, specs[i])
		}
	}
	return out
}

// unionStrings returns the de-duplicated concatenation of a and b, dropping
// empties and preserving first-seen order.
func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// remapDep slugifies a task's deps, rewrites any reference to `from` into `to`,
// drops self-references (`self`) and empties, and de-duplicates.
func remapDep(deps []string, from, to, self string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		s := stringutil.TaskSlug(d)
		if s == from {
			s = to
		}
		if s == "" || s == self || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// insertPlanQuestions persists the planner's questions and publishes ask
// events; pending older questions are superseded first.
func (w *Worker) insertPlanQuestions(qs []PlanQuestion) {
	_, _ = w.pdb.ExecWrite("UPDATE of_questions SET status = 'superseded', updated_at = datetime('now') WHERE status = 'pending'")
	for _, q := range qs {
		opts := "[]"
		if len(q.Options) > 0 {
			if jb, err := json.Marshal(q.Options); err == nil {
				opts = string(jb)
			}
		}
		qType := q.Type
		if qType == "" {
			qType = "text"
		}
		var secretName *string
		if q.SecretName != "" {
			secretName = &q.SecretName
		}
		var fieldsStr *string
		if len(q.Fields) > 0 && string(q.Fields) != "null" {
			s := string(q.Fields)
			fieldsStr = &s
		}
		qResult, err := w.pdb.ExecWrite(
			"INSERT INTO of_questions (question, urgency, status, options, type, secret_name, fields, origin) VALUES (?, 'normal', 'pending', ?, ?, ?, ?, ?)",
			q.Question, opts, qType, secretName, fieldsStr, QuestionOriginBuild,
		)
		if err != nil {
			w.logger.Warn("insert question failed", "error", err)
			continue
		}
		qID, _ := qResult.LastInsertId()
		w.publishEvent(events.EventQuestionAsked, map[string]any{
			"id":       qID,
			"question": q.Question,
			"options":  q.Options,
			"type":     qType,
		})
	}
}

// planBriefText is the brief PLAN reads on a fresh run: the expanded brief when EXPAND
// ran, else the owner's own.
func planBriefText(brief string, r *Run) string {
	if r != nil && r.ExpandedBrief != "" {
		return r.ExpandedBrief
	}
	return brief
}

// joinFeedback joins corrective messages, skipping empty ones.
func joinFeedback(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}
