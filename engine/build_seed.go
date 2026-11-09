package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/analysis"
	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// buildWholeSeed renders the whole-build seed messages: the owner's context, the intent
// + guides that stay byte-stable for the whole build (the cached prefix), and the
// instruction (the build checklist), placed in prompt-cache order. The project_state
// baseline is appended by the snapshot strategy, not here.
func (w *Worker) buildWholeSeed(plan *Plan, ownerAnswer string) []llm.Message {
	instruction := freshBuildInstruction(plan, ownerAnswer, w.changeRequest(), w.pdb.Reader())
	return seed{
		// Owner preferences (spelling, tone, defaults) apply to what BUILD writes as much
		// as to PLAN and LIVE; they change rarely, so the owner block stays cache-stable.
		owner:  w.ownerContext() + w.ownerMemoryBlock(),
		shared: w.buildSharedSeed(plan),
		stable: w.buildIntentSeed(plan),
		task:   instruction,
	}.messages()
}

// changeRequest returns the owner's change request when this run is an update of a
// live project, else "".
func (w *Worker) changeRequest() string {
	if w.run == nil || w.run.Kind != RunKindUpdate {
		return ""
	}
	return strings.TrimSpace(w.run.UpdateDescription)
}

// freshBuildInstruction is the whole-project build directive: build the entire
// plan in one continuous session, working the checklist in a sensible order and
// ticking items off as they land. changeRequest is non-empty on an update run: the
// re-plan re-seeds every item as open, so without the owner's actual request an item
// whose deliverables already exist reads as done and is completed with the change
// never applied.
func freshBuildInstruction(plan *Plan, ownerAnswer, changeRequest string, d *sql.DB) string {
	var b strings.Builder
	b.WriteString("[mode: build]\n\n")
	if ownerAnswer != "" {
		b.WriteString("The owner answered your question(s):\n" + ownerAnswer + "\n\n")
	}
	if changeRequest != "" {
		b.WriteString("This is a change to a project that is already live. The owner asked for:\n" + changeRequest + "\n\n")
		b.WriteString("The checklist below is the whole updated plan, so most items already exist. For each item, apply whatever part of this change touches it before you complete it; an item the change does not touch, whose deliverables already exist and are wired, is completed as-is. Do not rebuild what already works.\n\n")
	} else {
		b.WriteString("Build the whole project described by the checklist below, end to end, in this one continuous session.\n\n")
	}
	if len(plan.Exclusions) > 0 {
		b.WriteString("Out of scope (do NOT build): " + strings.Join([]string(plan.Exclusions), "; ") + "\n\n")
	}
	// The fresh-build opener, positioned as the last directive before the
	// checklist, the point the model acts from.
	if checklistAllResolved(d) {
		b.WriteString("Every item on the checklist below is already done or blocked: confirm nothing is left open, then stop.\n\n")
	} else {
		b.WriteString("The full checklist is already below - your first action is the create or save that starts its first item.\n\n")
	}
	b.WriteString(renderChecklist(d))
	return b.String()
}

// checklistAllResolved reports a non-empty checklist whose every item is done or blocked,
// the shape of a resume after the build already finished its work.
func checklistAllResolved(d *sql.DB) bool {
	all, err := LoadTasks(d)
	if err != nil || len(all) == 0 {
		return false
	}
	for _, t := range all {
		if t.Status != TaskDone && t.Status != TaskBlocked {
			return false
		}
	}
	return true
}

// renderChecklist renders the of_tasks queue as a build checklist, with each
// item's completion state, so the single agent knows what to build and which
// task ids to complete. Reflects status, so a resume shows what is already done.
func renderChecklist(d *sql.DB) string {
	all, err := LoadTasks(d)
	if err != nil || len(all) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Plan checklist\n")
	b.WriteString("Each item is one piece of the project; the ids in backticks already exist in your checklist - build each one and mark it complete by that id, don't re-add it. ")
	b.WriteString("`Owns` is what must exist when the item is done: page routes, tables and endpoint resources, plus the exact module files of a module split (`complete` is refused while any is missing); you choose the other file names. `Extends` paths are owned by another task and wired into here - modify them in place, don't recreate them.\n\n")
	// Modules any task extends (the owner included) are entries unless named as the
	// store; a task's exports never home in an entry, so the scaffold below must point
	// siblings at the task's store module instead (exportHomePaths). Computed once.
	editedByAnyTask := map[string]bool{}
	for _, t := range all {
		for _, e := range t.Edits {
			editedByAnyTask[normPlanPath(e)] = true
		}
	}
	for _, t := range all {
		box := "[ ]"
		switch t.Status {
		case TaskDone:
			box = "[x]"
		case TaskBlocked:
			box = "[blocked]"
		}
		fmt.Fprintf(&b, "- %s `%s` - %s", box, t.ID, t.Title)
		if intent := strings.TrimSpace(t.Intent); intent != "" {
			b.WriteString("\n  " + intent)
		}
		if dw := strings.TrimSpace(t.DoneWhen); dw != "" {
			b.WriteString("\n  Done when: " + dw)
		}
		if len(t.Paths) > 0 {
			b.WriteString("\n  Owns (must exist when done): " + strings.Join(t.Paths, ", "))
		}
		if len(t.Edits) > 0 {
			b.WriteString("\n  Extends (modify in place, owned by another task): " + strings.Join(t.Edits, ", "))
		}
		if len(t.Deps) > 0 {
			// The complete gate refuses an item whose deps are unfinished; naming the
			// edges here saves learning them from the refusal.
			b.WriteString("\n  After (deps; complete those first): " + strings.Join(t.Deps, ", "))
		}
		if t.Status == TaskDone {
			// A convention recorded at completion steers every later item.
			if c := strings.TrimSpace(t.Conventions); c != "" {
				b.WriteString("\n  Convention: " + c)
			}
		}
		if len(t.Exports) > 0 {
			// The leaf interface siblings import. Rendered for EVERY task in the always-
			// visible queue, so a consumer sees the exact accessor to `import` (never a
			// `window.__x` bridge) and the producer sees the contract it must export.
			b.WriteString("\n  Exposes (siblings import these; export them, and import a dep's instead of re-inventing): " + strings.Join(t.Exports, ", "))
			// Pre-assemble the exact import a sibling pastes to wire this interface in,
			// so the model never has to CONSTRUCT the line from the signatures (the step
			// where a weak model stubs a window.* bridge instead). The specifier is the
			// task's export HOME: its un-edited client module (the store), never the entry.
			if line := importScaffoldLine(t.Exports, exportHomePaths(t.Paths, editedByAnyTask)); line != "" {
				b.WriteString("\n    A sibling wires this in with: " + line)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// exportHomePaths orders a task's paths so the module its exports HOME in
// comes first, and never offers the page entry as that home: the entry is the
// unimported root, so a scaffold naming it tells every sibling to write the import
// the save gate rejects (entry_exports / cyclic_import). The home is, in order:
// a store-named module (`app.state.js`); else the first owned module no other task
// edits - except on a page-route task whose modules nobody edits, where that module
// IS the entry (leaves wired in through deps). With no home, a page-route task's
// JS paths are dropped so no scaffold is rendered; any other task keeps its order.
func exportHomePaths(paths []string, editedByAnyTask map[string]bool) []string {
	isJS := func(p string) bool {
		lp := strings.ToLower(strings.TrimSpace(p))
		return strings.HasSuffix(lp, ".js") || strings.HasSuffix(lp, ".mjs")
	}
	ownsRoute, anyEdited := false, false
	for _, p := range paths {
		if isPageRoutePath(p) {
			ownsRoute = true
		}
		if isJS(p) && editedByAnyTask[normPlanPath(p)] {
			anyEdited = true
		}
	}
	home := -1
	for i, p := range paths {
		if isJS(p) && isStoreModuleName(p) {
			home = i
			break
		}
	}
	if home < 0 && (!ownsRoute || anyEdited) {
		for i, p := range paths {
			if isJS(p) && !editedByAnyTask[normPlanPath(p)] {
				home = i
				break
			}
		}
	}
	if home < 0 {
		if !ownsRoute {
			return paths
		}
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			if !isJS(p) {
				out = append(out, p)
			}
		}
		return out
	}
	out := make([]string, 0, len(paths))
	out = append(out, paths[home])
	out = append(out, paths[:home]...)
	out = append(out, paths[home+1:]...)
	return out
}

// importScaffoldLine renders the ready-to-paste import a sibling uses to wire in a task's
// declared interface: the bare symbol names pulled from the task's owned JS module under
// its bare specifier (analysis.ProjectModuleName, what the importmap resolves). Returns ""
// when the task owns no JS module or exposes no usable symbol.
func importScaffoldLine(exports, paths []string) string {
	spec := ""
	for _, p := range paths {
		lp := strings.ToLower(p)
		if strings.HasSuffix(lp, ".js") || strings.HasSuffix(lp, ".mjs") {
			if n := analysis.ProjectModuleName(p); n != "" {
				spec = n
				break
			}
		}
	}
	if spec == "" {
		return ""
	}
	var names []string
	seen := map[string]bool{}
	for _, e := range exports {
		for _, n := range exportSymbolNames(e) {
			if n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("import { %s } from '%s';", strings.Join(names, ", "), spec)
}

// exportSymbolNames extracts every bare identifier from one exposed-interface
// entry. An entry normally carries one signature, but a packed entry joins
// several with ';' or '/'; a segment past the first counts only when it reads
// as a call signature, so prose inside a signature's description is never
// mistaken for a symbol.
func exportSymbolNames(entry string) []string {
	var parts []string
	for i, piece := range strings.Split(entry, ";") {
		piece = strings.TrimSpace(piece)
		if piece == "" {
			continue
		}
		if subs, ok := slashCallSplit(piece); ok {
			parts = append(parts, subs...)
			continue
		}
		if i > 0 && !isCallSignature(piece) {
			continue
		}
		parts = append(parts, piece)
	}
	var out []string
	for _, p := range parts {
		if n := exportSymbolName(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// slashCallSplit splits a piece on '/' when EVERY segment is a call signature
// (so "a(x)/b(y)" splits while "renders a/b/c" does not). Returns ok=false to
// leave the piece whole.
func slashCallSplit(piece string) ([]string, bool) {
	subs := strings.Split(piece, "/")
	if len(subs) < 2 {
		return nil, false
	}
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		s = strings.TrimSpace(s)
		if !isCallSignature(s) {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// isCallSignature reports whether s begins with an identifier immediately
// followed by an argument list.
func isCallSignature(s string) bool {
	s = strings.TrimSpace(s)
	n := exportSymbolName(s)
	if n == "" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(s[len(n):]), "(")
}

// exportSymbolName extracts the bare identifier from one exposed-interface entry (a
// planner-authored signature like "peerDistance(id, pos) -> number"): the leading run of
// identifier chars, stopping at the first '(' or space. Returns "" when it does not start
// with an identifier.
func exportSymbolName(sig string) string {
	sig = strings.TrimSpace(sig)
	end := 0
	for end < len(sig) {
		c := sig[end]
		isIdent := c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (end > 0 && c >= '0' && c <= '9')
		if !isIdent {
			break
		}
		end++
	}
	return sig[:end]
}

// buildSystemPrompt renders the byte-stable BUILD system prompt (role + body + ground
// rules + a deferred-tool index). The index covers only the tools this plan did NOT
// preload, each with its one-liner, so the model loads what it needs by name in ONE
// find_tool call instead of searching for it across several cache-busting turns.
func (w *Worker) buildSystemPrompt(plan *Plan) string {
	sel := pack.Select(w.effectivePacks(), StageBuild)
	extra := ""
	var packs []pack.Pack
	var reg *tools.Registry
	// Registry-derived blocks. Guarded so a zero Worker (tests) still renders the
	// note; a nil DB yields zero caps -> only ungated tools listed.
	var caps llm.Capabilities
	if w.deps != nil && w.deps.ToolRegistry != nil {
		reg = w.deps.ToolRegistry
		packs = w.effectivePacks()
		if w.deps.DB != nil {
			caps, _ = llm.LoadCapabilities(w.deps.DB.DB)
		}
		// The always-preloaded set's routing prose. A preloaded tool reaches the model as a
		// wire schema with only its 130-char one-liner; WhenToUse is emitted by find_tool,
		// which the ground rules forbid for these, so without this block that guidance never
		// arrives. Identical every task, so it rides the byte-stable prefix and is cached once;
		// a task's OWN extra tools go in the task message instead.
		// Mirrors buildAllTools: prose for a tool that is not loaded would leave the
		// guidance and the wire schema disagreeing.
		always := append([]string{}, tools.CoreBuildToolNames...)
		if planNeedsBackbone(plan) {
			always = append(always, tools.BuildBackboneToolNames...)
		}
		if prose := w.deps.ToolRegistry.RoutingProse(always, caps, StageBuild); prose != "" {
			extra += "## Your always-loaded tools\n\n" + prose + "\n\n"
		}
	}
	// One shared preamble (pack.ToolCataloguePreamble) for every rendered
	// DeferredIndex; this block only adds what is build-specific - the plan's
	// tools are the ones already loaded.
	extra += "## Deferred capabilities\n\n" +
		"Your plan's tools are already loaded. " + pack.ToolCataloguePreamble
	if reg != nil {
		if idx := reg.DeferredIndex(buildAllTools(plan), caps, StageBuild); idx != "" {
			extra += "\n\n" + idx
		}
	}
	conds, omit := w.buildPromptConditions(plan)
	return pack.Assemble(pack.AssembleInput{
		Stage:      StageBuild,
		Wave:       pack.Wave,
		Selection:  sel,
		Extra:      extra,
		Conditions: conds,
		OmitLaws:   omit,
		// Capability-dead guides are left out of every index: naming one
		// advertises tools that cannot be loaded, and the search costs turns.
		HiddenGuides: capabilityDeadGuides(packs, reg, caps),
	}).System
}

// buildSeedGuides resolves the guide slugs whose cores buildSharedSeed inlines,
// plus the capability-gap notes. One function used by both the seed render and
// the session spec (SeedGuides), so the two can never disagree about what is
// already in context. Deterministic for a given plan and install.
func (w *Worker) buildSeedGuides(plan *Plan) (map[string]bool, []string) {
	ps := w.effectivePacks()
	want := planGuideSlugs(plan, ps)
	var capNotes []string
	if len(want) > 0 && w.deps.DB != nil && w.deps.DB.DB != nil {
		// Capability-aware prefetch: warn inline about an implied guide whose tools are
		// capability-gated off, instead of letting the builder burn a find_tool round. Fail-
		// open on a load error: a build with an unchecked guide beats one missing its contract.
		if caps, err := llm.LoadCapabilities(w.deps.DB.DB); err != nil {
			w.logger.Warn("load capabilities failed; guide prefetch not capability-checked", "error", err)
		} else {
			want, capNotes = guideCapabilityGaps(want, ps, w.deps.ToolRegistry, caps)
		}
	}
	return want, capNotes
}

// buildSharedSeed renders the block a build carries that is NOT about this project: the
// auto-prefetched guide cores, the routing prose for the tools the plan pulled in, the
// install's capability notes. It leads the seed because it is the most widely shared text in
// the request. Guide cores come from a fixed corpus, so two projects that select the same
// guides carry the same bytes, and a rebuild of one project carries the same bytes as the
// build before it. Project-unique text sitting in front of it (as goal/voice/design used to)
// would put a few KB of never-repeated content ahead of tens of KB of repeatable content and
// make none of it shareable.
//
// Ordered most-shared first within the block too: corpus guides, then the plan's tool prose,
// then install notes, then the conditional window-bridge rule.
func (w *Worker) buildSharedSeed(plan *Plan) string {
	var b strings.Builder
	b.WriteString(buildSharedSentinel)
	b.WriteString("\nPlatform reference for this build. Not specific to this project; the project's own intent follows below.\n")

	ps := w.effectivePacks()
	want, capNotes := w.buildSeedGuides(plan)
	var locked map[string]string
	if plan != nil {
		locked = lockedDecisionChoices(plan.Decisions)
	}
	if block := renderGuidesFor(want, ps, locked); block != "" {
		b.WriteString("\n## Platform guides for this build\n\n")
		// The recall SYNTAX is deliberately absent: each guide block below names its own
		// extended sections with the exact call, and the guide-index pointer in the system
		// prompt covers the anything-else case. What only this block can say is how to READ
		// a guide - which half is binding and which is advisory - so that is all it says.
		b.WriteString("Auto-selected from your plan - each is the guide's CORE. The wire shapes and platform API rules in them are binding, reproduce those exactly; the rest is field notes from builds that failed, so follow those too unless you are certain your approach avoids the trap they mark.\n\n")
		b.WriteString(block)
		b.WriteString("\n")
	}

	// The reducer wire contract is an extended guide section (tick/relay builds
	// never carry it). Inline it up front whenever this build writes a reducer:
	// either the plan locked the authoritative model, OR a task pulled in
	// manage_authoritative (the create + set_reducer tool) without that exact
	// decision wording. Keyed on both signals so a reducer never ships without
	// its contract just because the decision was recorded differently.
	if plan != nil && (planLocksAuthoritative(plan.Decisions) || planWritesReducer(plan)) {
		b.WriteString("\n## Writing the reducer (authoritative endpoints)\n\n")
		lead := "This build writes an authoritative reducer."
		if planLocksAuthoritative(plan.Decisions) {
			lead = "Your plan locked `realtime model = authoritative`."
		}
		b.WriteString(lead + " The reducer's contract follows - read it before writing one; the sandbox deliberately throws on things that work everywhere else. Worked example, seeding, matchmaking handoff, hidden-info `view()`: recall the realtime_signaling guide's `authoritative-games` section.\n\n")
		b.WriteString(pack.TranscludeContracts("{{contract:room-reducer}}"))
		b.WriteString("\n")
	}

	// Routing prose for the tools this plan pulled in beyond the always-loaded set. A
	// preloaded tool reaches the model as its 130-char one-liner only (WhenToUse is emitted
	// by find_tool, which the model is told not to call for these), so the relay-vs-tick-vs-
	// authoritative disambiguation must be delivered here. Plan-derived and stable, so cached once.
	if w.deps != nil && w.deps.ToolRegistry != nil {
		var caps llm.Capabilities
		if w.deps.DB != nil && w.deps.DB.DB != nil {
			caps, _ = llm.LoadCapabilities(w.deps.DB.DB)
		}
		if prose := w.deps.ToolRegistry.RoutingProse(planExtraTools(plan), caps, StageBuild); prose != "" {
			b.WriteString("\n## Tools this plan loaded for you\n\n")
			b.WriteString("This is when to reach for each over its neighbours.\n\n")
			b.WriteString(prose)
			b.WriteString("\n")
		}
	}

	if len(capNotes) > 0 {
		b.WriteString("\n## Unavailable in this configuration\n")
		b.WriteString("These are settled facts about this install, not obstacles to work around. Each names what to do instead; do it, and never spend a turn searching for the missing tool.\n")
		for _, n := range capNotes {
			b.WriteString("- " + n + "\n")
		}
	}
	// A plan below the full client_js_split threshold can still ship two sibling client-JS
	// files that need to share a symbol. There the split guide is not injected, so without
	// this the model defaults to a `window.*` bridge the save gate rejects only reactively.
	// Carry the compact window-bridge rule up front in that case (when the full guide is
	// present it already covers this, and planNeedsWindowBridgeRule returns false).
	if planNeedsWindowBridgeRule(plan, ps) {
		b.WriteString("\n" + windowBridgeRuleBlock)
	}
	return b.String()
}

// lockedDecisionChoices maps each locked decision topic to its choice, both
// lowercased, for the decision-aware guide core render.
func lockedDecisionChoices(decisions []PlanDecision) map[string]string {
	if len(decisions) == 0 {
		return nil
	}
	out := make(map[string]string, len(decisions))
	for _, d := range decisions {
		t := strings.ToLower(strings.TrimSpace(d.Topic))
		c := strings.ToLower(strings.TrimSpace(d.Choice))
		if t != "" && c != "" {
			out[t] = c
		}
	}
	return out
}

// planRealtimeChoice returns the locked "realtime model" decision's choice in
// lowercase (topic and choices as the realtime guide's decision block names
// them), or "" when the plan carries no such decision.
func planRealtimeChoice(decisions []PlanDecision) string {
	for _, d := range decisions {
		if isRealtimeModelTopic(d.Topic) {
			if canon := normalizeRealtimeChoice(d.Choice); canon != "" {
				return canon
			}
			return strings.ToLower(strings.TrimSpace(d.Choice))
		}
	}
	return ""
}

// planLocksAuthoritative reports whether the plan locked the realtime model
// decision to the authoritative reducer.
func planLocksAuthoritative(decisions []PlanDecision) bool {
	return planRealtimeChoice(decisions) == "authoritative"
}

// planWritesReducer reports whether any plan task pulled in manage_authoritative,
// the create + set_reducer tool. It is a second, decision-independent signal that
// this build writes a reducer, so the room-reducer contract rides the seed even
// when the realtime model decision was recorded with wording the normalizer misses.
func planWritesReducer(plan *Plan) bool {
	for _, name := range planExtraTools(plan) {
		if name == "manage_authoritative" {
			return true
		}
	}
	return false
}

// buildIntentSeed renders what is true of THIS project and nothing else: goal, voice, design
// direction, and the locked decisions. Byte-stable across every task of the build; the shared
// platform reference is buildSharedSeed's, ahead of this block.
func (w *Worker) buildIntentSeed(plan *Plan) string {
	var b strings.Builder
	b.WriteString(buildIntentSentinel)
	b.WriteString("\nProject intent for this build (stable for the whole build).\n")
	if g := strings.TrimSpace(plan.Goal); g != "" {
		b.WriteString("\n## Goal\n" + g + "\n")
	}
	if v := strings.TrimSpace(plan.Voice); v != "" {
		b.WriteString("\n## Voice\n" + v + "\n")
	}
	if t := strings.TrimSpace(plan.Theme); t != "" {
		b.WriteString("\n## Theme (what the look must fit)\n" + t + "\n")
	}
	if d := strings.TrimSpace(plan.Design); d != "" {
		b.WriteString("\n## Design direction\n" + d + "\n")
	}
	// The same look as values. Prose survives PLAN but reaches a build as something to
	// re-derive, and a look re-derived per project is how distinct briefs land on one
	// template; these are what the global stylesheet is actually built from. Byte-stable
	// like the rest of this seed: the plan is fixed for the run and the fields marshal in
	// declaration order.
	if ds := plan.DesignSystem; !ds.isEmpty() {
		if raw, err := json.Marshal(ds); err == nil {
			b.WriteString("\n## Design system (the committed look as values)\n")
			b.WriteString("Build the global stylesheet from these: each becomes a `:root` custom property, and every page and component styles from `var(--token)` rather than a fresh hex or px. Extend them where a screen earns more (a tonal step, a state colour); never swap in a different palette or family.\n")
			b.WriteString("```json\n")
			b.Write(raw)
			b.WriteString("\n```\n")
		}
	}
	// The full brief rides the seed so BUILD can judge depth against what was
	// actually described, not only the plan's distillation of it.
	if brief := w.buildBrief(); brief != "" {
		b.WriteString("\n## Project brief (the product being built)\n")
		b.WriteString("The checklist below is this brief distilled into tasks. When an item reads thinner than the behaviour described here, build what the brief describes.\n\n")
		b.WriteString(brief + "\n")
	}
	// The brief's features and the items that build each, with the excluded ones
	// marked, so a brief that still describes a cut feature is not read as an order.
	b.WriteString(renderFeaturesForBuild(plan))
	// Locked architecture decisions rendered as binding constraints (see
	// renderLockedDecisions): the plan chose these with the full guides in front of it,
	// and the decision-conformance validator reports it if an artifact diverges.
	b.WriteString(renderLockedDecisions(plan.Decisions))
	return b.String()
}

// buildBrief returns the brief BUILD rides: the expanded brief when EXPAND ran,
// else the owner's raw description on a fresh build. Update runs get neither
// (their intent lives in the updated plan, and a stale brief would fight it).
// Stable for the whole build: the expanded brief is persisted before BUILD starts.
func (w *Worker) buildBrief() string {
	if w.run == nil {
		return ""
	}
	if e := strings.TrimSpace(w.run.ExpandedBrief); e != "" {
		return e
	}
	if w.run.Kind == RunKindUpdate {
		return ""
	}
	return strings.TrimSpace(w.projectBrief())
}

// renderLockedDecisions renders the plan's locked architecture decisions as the binding-
// constraints block of the build intent seed. A decision's machine `check` is annotated
// "(verified: ...)" ONLY when the conformance validator can actually enforce it (resolvable
// artifact/field AND an op it evaluates), so it never promises an enforcement that never
// runs. Pure over the decisions, so the annotation logic is unit-testable.
func renderLockedDecisions(decisions []PlanDecision) string {
	if len(decisions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Locked architecture decisions (build these EXACTLY)\n")
	b.WriteString("These were chosen at plan time and are binding. One marked (verified: ...) is checked after the build and a deviation is reported to the owner as a known issue; the rest are not checked but still bind you:\n")
	for _, dec := range decisions {
		topic := strings.TrimSpace(dec.Topic)
		choice := strings.TrimSpace(dec.Choice)
		if topic == "" && choice == "" {
			continue
		}
		fmt.Fprintf(&b, "- **%s** = `%s`", topic, choice)
		if r := strings.TrimSpace(dec.Rationale); r != "" {
			fmt.Fprintf(&b, " - %s", r)
		}
		if dec.Check != nil && strings.TrimSpace(dec.Check.Field) != "" &&
			decisionCheckResolvable(dec.Check.Artifact, dec.Check.Field) && validDecisionOps[dec.Check.Op] {
			fmt.Fprintf(&b, " (verified: %s `%s`.%s %s %v)", dec.Check.Artifact, dec.Check.ID, dec.Check.Field, dec.Check.Op, dec.Check.Value)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// windowBridgeRuleBlock is the compact symbol-sharing rule injected into the intent seed
// for a multi-JS plan that does not pull the full client_js_split guide (see
// planNeedsWindowBridgeRule). Its wording tracks the window_global_bridge gate's reject.
const windowBridgeRuleBlock = "## Sharing a symbol across client-JS files\n" +
	"This build has more than one client-JS module. When two files need the same symbol - a helper, a store, a formatter, a CDN library global - share it through the module import graph, NEVER a `window.*` global of your own. Put it in a zero-import store module (a file that imports no other project module), `export` it there, and `import` it in every file that uses it. A cross-file `window.X` read is rejected at write (`window_global_bridge` when a sibling assigns it, `window_dead_global` when nothing does): it carries no import edge, so it ships wiring that resolves nowhere and can boot silently dead while every gate and probe stays green. Only `window.__routeParams` and `window.__ofProbeViolations` are exempt.\n"

// buildIntentSentinel marks the cached intent block in logs.
const buildIntentSentinel = "<!--__of_build_intent__-->"

// buildSharedSentinel marks the cached platform-reference block in logs. Separate from the
// intent sentinel so triage can tell the shared corpus apart from the project's own text.
const buildSharedSentinel = "<!--__of_build_shared__-->"

// buildPromptConditions says which of the build frame's project-specific rules apply
// (its `<!-- when:NAME -->` blocks) and which platform laws cannot, so a one-page tool
// does not re-read the auth helpers, the REST envelope, the module-split order and the
// headless rules on every turn. A plan with no tasks says nothing: every rule stays.
func (w *Worker) buildPromptConditions(plan *Plan) (map[string]bool, map[string]bool) {
	if plan == nil || len(plan.Tasks) == 0 {
		return nil, nil
	}
	data := planNeedsBackbone(plan)
	auth := planHasAuthSurface(plan)
	conds := map[string]bool{
		"headless":  !planHasUI(plan),
		"split":     planImpliesClientSplit(plan) || planClientJSFiles(plan) >= 2,
		"data":      data,
		"auth":      auth,
		"schedules": planMentionsSchedules(plan),
	}
	if w.deps != nil && w.deps.DB != nil {
		// The flow-writing guidance rides only the flow-gate session, whose ephemeral plan
		// copy carries the user-flows task; the main build never does, so it is not told to
		// write of_test.js (flows are a post-build gate).
		conds["flows"] = w.userFlowsEnabled() && planHasUserFlowsTask(plan)
	}
	omit := map[string]bool{"api-response": !data, "auth-window-api": !auth, "trust-boundary": !data}
	return conds, omit
}

// planMentionsSchedules reports whether any task schedules work: it declares the
// schedules tool or talks about a recurring job.
func planMentionsSchedules(plan *Plan) bool {
	for _, t := range plan.Tasks {
		for _, n := range t.Tools {
			if strings.TrimSpace(n) == "manage_schedules" {
				return true
			}
		}
		text := taskText(t)
		for _, kw := range []string{"schedul", "cron", "recurring", "every day", "every hour", "nightly", "daily digest", "reminder"} {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}
