package engine

// Plan-time coherence checks: the deterministic pass that catches a plan at odds
// with itself or with the pipeline before BUILD, where the problem surfaces only
// as a real defect or wasted build turns. A violation drives ONE bounded re-plan turn
// (planCoherenceFeedback) and never hard-fails. The checks are PURE over the
// parsed plan, CONSERVATIVE (they fire only on an unambiguous contradiction),
// and unit-testable.

import (
	"fmt"
	"sort"
	"strings"
)

// subsystemLeafThreshold is how many subsystem tasks must funnel into one shared
// entry module before the plan is flagged. Two edit-tasks on one entry fits one
// save; three or more overruns the size cap mid-build and forces a re-split.
const subsystemLeafThreshold = 3

// planCoherenceFeedback returns a corrective-retry message naming every internal
// contradiction in the plan (all at once, as one re-plan turn), or "" when
// coherent. projectHasAuth suppresses ONLY the owner-scope-without-auth check:
// an update run legitimately omits an auth surface the live project already has.
// The other checks stay on for updates.
func planCoherenceFeedback(p *Plan, projectHasAuth bool) string {
	if p == nil {
		return ""
	}
	var problems []string
	if !projectHasAuth {
		if m := ownerScopeWithoutAuthProblem(p); m != "" {
			problems = append(problems, m)
		}
	}
	if m := subsystemsWithoutLeavesProblem(p); m != "" {
		problems = append(problems, m)
	}
	if m := entryExportsProblem(p); m != "" {
		problems = append(problems, m)
	}
	if m := appShapeRouteSprawlProblem(p); m != "" {
		problems = append(problems, m)
	}
	if m := realtimeModelContradictionProblem(p); m != "" {
		problems = append(problems, m)
	}
	if m := missingRealtimeDecisionProblem(p); m != "" {
		problems = append(problems, m)
	}
	if m := platformOwnedTaskProblem(p); m != "" {
		problems = append(problems, m)
	}
	if !projectHasAuth {
		if m := pageOnlyGateProblem(p); m != "" {
			problems = append(problems, m)
		}
	}
	if len(problems) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Your plan has internal contradictions that would ship broken or thrash the build. Fix EACH and re-emit the FULL plan JSON, keeping every other field intact. Output only the JSON object, no prose, no markdown fences.")
	for _, m := range problems {
		b.WriteString("\n\n- ")
		b.WriteString(m)
	}
	return b.String()
}

// ownerScopeWithoutAuthProblem fires when the plan LOCKS an owner/tenant/role
// data-ownership decision yet no task provides a login surface: that endpoint
// 401s every write, and BUILD's only escape is to flip it public - silently
// discarding the promised per-user ownership. The coherent resolutions are
// surfaced so the re-plan picks one deliberately.
func ownerScopeWithoutAuthProblem(p *Plan) string {
	if !planHasScopedOwnershipDecision(p) || planHasAuthSurface(p) {
		return ""
	}
	return "A `data ownership` decision scopes rows to a logged-in user (owner/authored/tenant/role/authenticated), but no task creates a login surface (no `create_auth`/`create_oauth`), so every write to that endpoint 401s in the browser - the feature is dead on arrival. Resolve it ONE of three ways: (1) add an auth-surface task (a user table + `create_auth`) so users sign in and see only their own rows; (2) if the data is personal to a single no-login user and only needs to survive reloads on the same device (a personal library, drafts, preferences), drop the table and persist client-side in localStorage - state the store in the page task's intent and declare NO table; or (3) if the data really is shared by everyone, change the decision to `public`. Do NOT keep an owner-scoped server table with no login."
}

// planHasScopedOwnershipDecision reports whether any locked decision scopes data
// to a user/tenant/role (the choices that REQUIRE authentication), matched
// loosely on the topic. `authored` belongs with `owner`: it shares READS but
// still needs an owner_column to scope writes, and the runtime 401s an
// unauthenticated caller on any endpoint that sets one.
func planHasScopedOwnershipDecision(p *Plan) bool {
	for _, d := range p.Decisions {
		topic := strings.ToLower(strings.TrimSpace(d.Topic))
		if !strings.Contains(topic, "ownership") && !strings.Contains(topic, "access model") {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(d.Choice)) {
		case "owner", "authored", "tenant", "role", "authenticated":
			return true
		}
	}
	return false
}

// planHasAuthSurface reports whether any task plans a login surface. Generous by
// design: any auth tool OR any login/register wording counts, so this check errs
// toward "has auth" and the owner-scope problem fires ONLY when there is clearly no
// login anywhere - a coherent plan is never sent back.
func planHasAuthSurface(p *Plan) bool {
	for _, t := range p.Tasks {
		for _, tool := range t.Tools {
			switch strings.ToLower(strings.TrimSpace(tool)) {
			case toolAuth, toolOAuth, "create_auth", "create_oauth":
				return true
			}
		}
		text := taskText(t)
		for _, kw := range []string{"create_auth", "create_oauth", "log in", "login", "sign in", "sign-in", "sign up", "sign-up", "authenticat", "register account", "user account"} {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

// subsystemsWithoutLeavesProblem fires when many tasks EDIT one shared entry (a
// client module OR the page route) while owning no client module of their own:
// the funnel overruns the save/read size cap mid-build and forces a reactive
// re-split. Routes count because a doctrine-following planner names no internal
// .js file, so an app-like page's fan-out shows up as N tasks editing one ROUTE.
func subsystemsWithoutLeavesProblem(p *Plan) string {
	entry, n, ok := fanoutEntry(p, subsystemLeafThreshold)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d tasks funnel into the shared entry %q but own no client module of their own, so it grows past the save-size cap and forces a mid-build re-split that scatters shared symbols across ad-hoc files. Split this application-like page: ONE spine task OWNs the route and the entry module `/files/<app>.js`; the shared contract lives in a zero-import STORE module `/files/<app>.state.js` (the store.js pattern - own it in the spine task alongside the entry, or as its own task); then group the subsystems into a few cohesive leaf modules (each under the client-JS size cap), each OWNed by its own task that imports the store and only `edits` the entry to wire in. Do not funnel every subsystem through one file - but don't swing the other way into a dozen thin leaves either; let cohesion and the size cap set the count.", n, entry)
}

// entryExportsProblem fires when the ENTRY task of a subsystem split declares
// `exports` with nowhere valid to home them. The entry is the unimported ROOT
// of the import tree, so exported symbols close a cycle (leaf -> entry -> leaf)
// the save-time cyclic gate rejects on the FIRST leaf; shared symbols belong in
// a ZERO-IMPORT STORE module. A task that owns the entry AND such a store (an
// owned client module no other task edits) is VALID: its exports home in the
// store, which is the model's natural one-core-task plan shape (see
// taskExportHome). Conservative: fires only when the exporting task owns
// route+module, another task edits it, and no home exists. Deterministic
// (first offender).
func entryExportsProblem(p *Plan) string {
	if p == nil {
		return ""
	}
	for i := range p.Tasks {
		t := p.Tasks[i]
		if len(t.Exports) == 0 || !taskOwnsPageRoute(t) {
			continue
		}
		entryModule, entryDisplay := spineEntryModule(p, i)
		if entryModule == "" {
			continue
		}
		// A home for the exports: another owned client module no task edits
		// (the zero-import store). Owning one makes this plan shape valid -
		// the exports live there, not in the entry file - so don't flag it.
		// normalizeEntryExports (plan_normalize.go) adds that store module
		// deterministically before this check, so the common slip - exports on an
		// entry with no store - never reaches here as a full re-plan turn.
		if taskExportHome(p, i, entryModule) != "" {
			continue
		}
		return fmt.Sprintf("the entry task %q owns the app entry %q AND declares `exports` (%s), but owns no other module to home them in. The entry is the unimported ROOT of the import tree - nothing may import it - so every leaf that wires into it would `import` those symbols and close a cycle (`leaf -> %s -> leaf`), which the save gate rejects on the FIRST leaf and forces a whole-entry refactor mid-build. Shared state, DOM refs, and cross-leaf helpers belong in a ZERO-IMPORT STORE module (the store.js pattern), never the entry file. Fix, either way works: ADD the store module %q to this task's `paths` and keep the `exports` (they home there; the entry and every leaf import them from it), OR split the store into its own task (no page route, no `deps`) carrying these `exports` and CLEAR this task's. Re-emit the full plan.",
			planTaskSlug(t), entryDisplay, strings.Join(t.Exports, "; "), entryDisplay, stateLeafPath(entryDisplay))
	}
	return ""
}

// taskOwnsPageRoute reports whether the task OWNs a page route - the mark of a
// spine/entry task, as opposed to a leaf or a data/endpoint task.
func taskOwnsPageRoute(t PlanTaskSpec) bool {
	for _, pth := range t.Paths {
		if isPageRoutePath(pth) {
			return true
		}
	}
	return false
}

// leafEditedEntry returns the client module task i OWNs that ANOTHER task EDITS -
// the entry that leaves wire INTO - as (normalized, display) paths, or "","" when
// the task owns no such module. Picking the leaf-edited module (NOT just the last
// client path) means a task owning BOTH the entry and a zero-import store (app.js
// AND app.state.js) is judged by its entry: the store, which no leaf edits, is
// never mistaken for it. A store-NAMED module is never the entry even when leaves
// edit it (they add their exports to the shared store): taking it for the entry
// derived a second store `app.state.state.js` beside the real one.
func leafEditedEntry(p *Plan, i int) (norm, display string) {
	for _, pth := range p.Tasks[i].Paths {
		if !isClientModulePath(pth) || isStoreModuleName(pth) {
			continue
		}
		n := normPlanPath(pth)
		for j := range p.Tasks {
			if j != i && taskEditsModule(p.Tasks[j], n) {
				return n, strings.TrimSpace(pth)
			}
		}
	}
	return "", ""
}

// spineEntryModule returns the entry module of spine task i: the leaf-edited module
// when leaves wire in by editing it, else - when leaves wire in through deps and
// imports alone, so nothing edits the entry - the task's one non-store client module,
// provided the page is split (another task owns a client module). "","" when neither
// holds. Shared by entryExportsProblem and normalizeEntryExports so the guard and the
// normalizer identify the entry identically; before the deps-wired case was covered,
// a spine exporting from its entry passed both unflagged and the checklist told every
// leaf to import from the entry, which the save gate then rejected.
func spineEntryModule(p *Plan, i int) (norm, display string) {
	if n, d := leafEditedEntry(p, i); n != "" {
		return n, d
	}
	var only string
	for _, pth := range p.Tasks[i].Paths {
		if !isClientModulePath(pth) || isStoreModuleName(pth) {
			continue
		}
		if only != "" {
			return "", "" // several candidates: not a shape to guess through
		}
		only = pth
	}
	if only == "" {
		return "", ""
	}
	for j := range p.Tasks {
		if j != i && taskOwnsClientModule(p.Tasks[j]) {
			return normPlanPath(only), strings.TrimSpace(only)
		}
	}
	return "", ""
}

// storeModuleSuffixes name the zero-import store module by convention
// (`app.state.js`, `app.store.js`, `app.core.js`).
var storeModuleSuffixes = []string{".state", ".store", ".core"}

// isStoreModuleName reports whether a client module path is named as the page's
// zero-import store module.
func isStoreModuleName(p string) bool {
	base := strings.ToLower(strings.TrimSpace(p))
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".js"), ".mjs")
	for _, s := range storeModuleSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

// taskExportHome returns the normalized path of task i's owned client module
// that no OTHER task edits (its zero-import store), excluding the entry, or ""
// when none exists. This is where the task's `exports` home: exports never
// live in the entry file, and a task owning entry + store is judged valid on
// that basis (the checklist scaffold points siblings at the store too).
func taskExportHome(p *Plan, i int, entryModule string) string {
	t := p.Tasks[i]
	for _, pth := range t.Paths {
		if !isClientModulePath(pth) {
			continue
		}
		norm := normPlanPath(pth)
		if norm == entryModule {
			continue
		}
		edited := false
		for j := range p.Tasks {
			if j != i && taskEditsModule(p.Tasks[j], norm) {
				edited = true
				break
			}
		}
		if !edited {
			return norm
		}
	}
	return ""
}

// stateLeafPath derives the conventional zero-import store module path from the
// entry module path: /files/game.js -> /files/game.state.js. A path already named as
// a store is returned unchanged, never doubled into game.state.state.js.
func stateLeafPath(entry string) string {
	e := strings.TrimSpace(entry)
	if isStoreModuleName(e) {
		return e
	}
	if strings.HasSuffix(strings.ToLower(e), ".js") {
		return e[:len(e)-len(".js")] + ".state.js"
	}
	return e + ".state.js"
}

// fanoutEntry finds a shared ENTRY (a client module or page route OWNED by some
// task) that at least `min` tasks EDIT while owning no client module themselves.
// Deterministic, and shared by the coherence guard and the client-split guide
// selector so both treat route and .js-entry funnels alike.
func fanoutEntry(p *Plan, min int) (entry string, n int, ok bool) {
	if p == nil {
		return "", 0, false
	}
	owned := map[string]string{} // normalized -> display path
	for _, t := range p.Tasks {
		for _, pth := range t.Paths {
			if isClientModulePath(pth) || isPageRoutePath(pth) {
				owned[normPlanPath(pth)] = strings.TrimSpace(pth)
			}
		}
	}
	norms := make([]string, 0, len(owned))
	for k := range owned {
		norms = append(norms, k)
	}
	sort.Strings(norms) // deterministic: same plan -> same entry chosen
	for _, e := range norms {
		c := 0
		for _, t := range p.Tasks {
			if taskEditsModule(t, e) && !taskOwnsClientModule(t) {
				c++
			}
		}
		if c >= min {
			return owned[e], c, true
		}
	}
	return "", 0, false
}

// isClientModulePath reports whether a planner path is a client JS module
// (a /files/*.js or *.mjs asset), reusing normPlanPath's /files/ + case folding.
func isClientModulePath(p string) bool {
	n := normPlanPath(p)
	return strings.HasSuffix(n, ".js") || strings.HasSuffix(n, ".mjs")
}

// isPageRoutePath reports whether a planner path is a page route (a root-absolute
// path that is NOT a /files/ asset) rather than a client module or a bare table/API
// resource name. normPlanPath keeps page routes in the slash namespace, so a
// normalized path that still starts with "/" is a route.
func isPageRoutePath(p string) bool {
	return strings.HasPrefix(normPlanPath(p), "/")
}

// isRootedNonAsset is the shared core of the page-route checks: a path that is
// rooted ("/...") and not a /files/ asset. Callers that also count the shared
// layout key "default" as a page OR it in explicitly. Distinct from isPageRoute
// (which also excludes /api/ and requires no file extension) and isPageRoutePath
// (which normalizes first).
func isRootedNonAsset(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "/files/")
}

// taskEditsModule reports whether the task's edits include the given normalized
// module path.
func taskEditsModule(t PlanTaskSpec, normModule string) bool {
	for _, e := range t.Edits {
		if normPlanPath(e) == normModule {
			return true
		}
	}
	return false
}

// taskOwnsClientModule reports whether the task owns (in its paths) any client JS
// module of its own.
func taskOwnsClientModule(t PlanTaskSpec) bool {
	for _, pth := range t.Paths {
		if isClientModulePath(pth) {
			return true
		}
	}
	return false
}

// appShapeRouteSprawlProblem fires when the plan LOCKS `project shape = app` yet
// still carves multiple screens into their own page routes. Auth and
// error/offline pages are exempt. Conservative: one stray route may be a
// judgment call and is let through; two or more is unambiguous sprawl.
func appShapeRouteSprawlProblem(p *Plan) string {
	if !planLocksAppShape(p) {
		return ""
	}
	exempt := map[string]bool{
		"/": true, "/login": true, "/signup": true, "/reset": true,
		"/404": true, "/offline": true, "/privacy": true, "/terms": true,
	}
	// Collect DISTINCT routes: one route named by two tasks (the page and the task
	// that edits it) is one route, and the threshold below counts routes, not
	// mentions. Same owned-map keying as fanoutEntry.
	owned := map[string]string{} // normalized -> display path
	for _, t := range p.Tasks {
		for _, pth := range t.Paths {
			if isPageRoutePath(pth) && !isClientModulePath(pth) && !exempt[normPlanPath(pth)] {
				owned[normPlanPath(pth)] = strings.TrimSpace(pth)
			}
		}
	}
	if len(owned) < 2 {
		return ""
	}
	sprawl := make([]string, 0, len(owned))
	for _, display := range owned {
		sprawl = append(sprawl, display)
	}
	sort.Strings(sprawl)
	return fmt.Sprintf("The plan locks `project shape = app` (one page at \"/\", screens as client-side views) but tasks still own the extra page routes %s. An app's screens are VIEWS, not routes: fold each of those screens into a view leaf task of the \"/\" page (`paths: [\"/files/<app>.<view>.js\"]`, `edits` the shared entry, `deps` on the spine) and switch views client-side. Keep a separate route only for the standalone auth screens (/login, /signup, /reset), /404, /offline, and the legal pages (/privacy, /terms) - or, if this project genuinely needs routed pages, change the decision to `site`.", strings.Join(sprawl, ", "))
}

// planLocksAppShape reports whether a locked decision declares the project
// app-shaped, matched loosely on topic and choice. It reads the leading label,
// not the whole choice: a rationale tail naming the rejected option ("site - not
// a single-page app") would otherwise demote to "no shape locked".
func planLocksAppShape(p *Plan) bool {
	for _, d := range p.Decisions {
		topic := strings.ToLower(strings.TrimSpace(d.Topic))
		if !isProjectShapeTopic(topic) {
			continue
		}
		choice := strings.ToLower(strings.TrimSpace(d.Choice))
		if strings.HasPrefix(choice, "site") {
			return false
		}
		if strings.Contains(choice, "app") {
			return true
		}
	}
	return false
}

// realtimeModelContradictionProblem fires when the plan LOCKS a NON-authoritative
// realtime model (`tick` or `relay`, endpoints that run no server code) yet a
// task relies on a server-side reducer, which only an `authoritative` endpoint
// has - BUILD would burn turns or silently drop that server logic. Conservative:
// an `authoritative` lock clears it, a `matchmaking` lock never trips it, and
// only the platform's reducer vocabulary counts as evidence, so a correct
// "non-authoritative tick" task is never sent back. Deterministic (first offender).
func realtimeModelContradictionProblem(p *Plan) string {
	if p == nil {
		return ""
	}
	nonAuthoritative := false
	for _, d := range p.Decisions {
		if !isRealtimeModelTopic(d.Topic) {
			continue
		}
		// Read the leading label so a rationale tail ("tick - continuous state")
		// still classifies (normalizeRealtimeChoice; the same reader planRealtimeChoice uses).
		switch normalizeRealtimeChoice(d.Choice) {
		case "authoritative":
			return "" // authoritative model -> a server reducer is exactly right
		case "tick", "relay":
			nonAuthoritative = true
		}
	}
	if !nonAuthoritative {
		return ""
	}
	for i := range p.Tasks {
		if taskImpliesServerReducer(p.Tasks[i]) {
			return fmt.Sprintf("the `realtime model` decision locks a NON-authoritative endpoint (`tick`/`relay`, which runs NO server code and validates NOTHING) but task %q relies on a server-side reducer: a `manage_tick`/`manage_relay` endpoint cannot own a roster, assign a value no client sent, or validate/expire state server-side. Resolve it ONE of two ways: (1) if the shared state is genuinely contested, cheat-sensitive, or hidden-info, change the decision to `authoritative` and build the endpoint with `manage_authoritative` + `set_reducer` (it may ALSO set `tick_hz` to simulate continuously); (2) if it is not - cosmetic assignment, presence, a roster - drop the reducer: assign client-side on join, drive presence from the platform's `net.onPeers` roster and the welcome/join/leave frames, and carry discrete events on `net.event`. The `realtime_signaling` guide's realtime-model Decision draws this line; do NOT describe a server reducer on a `tick`/`relay` endpoint.", planTaskSlug(p.Tasks[i]))
		}
	}
	return ""
}

// missingRealtimeDecisionProblem fires when a task loads a realtime wire tool but the plan
// records no `realtime model` decision. The plan prompt requires that decision, and
// everything downstream keys on it: the conformance check, the guide signal and the
// pruning of the other realtime tools. Without it the build is not held to the model the
// task chose, and a keyword-inferred schema for a rival model rides every turn.
func missingRealtimeDecisionProblem(p *Plan) string {
	if p == nil {
		return ""
	}
	for _, d := range p.Decisions {
		if isRealtimeModelTopic(d.Topic) {
			return ""
		}
	}
	for _, t := range p.Tasks {
		for _, tool := range t.Tools {
			name := strings.ToLower(strings.TrimSpace(tool))
			if isRealtimeModelTool(name) {
				return fmt.Sprintf("task %q loads the realtime tool `%s` but the plan records no `realtime model` decision. Add one to `decisions` (topic \"realtime model\", choice relay|tick|authoritative|matchmaking|webrtc, a one-line rationale, and the exact `check` the realtime_signaling guide's Decision block gives for that choice), so the build is held to the model this task picked.", planTaskSlug(t), name)
			}
		}
	}
	return ""
}

// taskImpliesServerReducer reports whether a task relies on the platform's
// authoritative server reducer - by loading `manage_authoritative` or naming the
// reducer as a DEPENDENCY in its prose. The subtlety: a careful non-authoritative
// plan spells out that it uses NO reducer ("NO server reducer", "without a
// reducer"), and that disclaimer contains the word "reducer" too. A plain
// substring match reads the disclaimer as a dependency and sends a correct plan
// back to a fix it cannot make (the very reason a coherence issue then survives
// the retry), so the reducer signal must be negation-aware. Matched on the task's
// tools plus its title/intent/done_when.
func taskImpliesServerReducer(t PlanTaskSpec) bool {
	for _, tool := range t.Tools {
		if strings.EqualFold(strings.TrimSpace(tool), toolAuthoritative) {
			return true
		}
	}
	text := taskText(t)
	if strings.Contains(text, toolAuthoritative) {
		return true
	}
	return mentionsServerReducer(text)
}

// reducerNegationCues, found in the clause just before a "reducer" mention, mark
// it a disclaimer (the plan rules the reducer OUT) rather than a dependency.
// taskText lowercases, so these are lowercase; the leading space makes a cue
// match on a word boundary ("casino" never reads as "no").
var reducerNegationCues = []string{
	" no ", " no-", " not ", " without", " never", " sans ", " zero ",
	" avoid", " drop the", " skip", " instead of", " rather than",
}

// mentionsServerReducer reports whether text names a server reducer as a real
// DEPENDENCY, not a disclaimer. Each "reducer" occurrence counts only when it is
// not the "reducer-less" form and is not negated by a cue in the same clause just
// before it - so "NO server reducer" (a correct tick plan being explicit) does not
// read as reliance on one.
func mentionsServerReducer(text string) bool {
	const needle = "reducer"
	const window = 32 // covers "no server[-side] reducer" and similar
	for i := 0; ; {
		j := strings.Index(text[i:], needle)
		if j < 0 {
			return false
		}
		at := i + j
		i = at + len(needle)
		if rest := text[i:]; strings.HasPrefix(rest, "less") || strings.HasPrefix(rest, "-less") {
			continue // "reducerless" / "reducer-less" is itself a disclaimer
		}
		start := at - window
		if start < 0 {
			start = 0
		}
		seg := text[start:at]
		if k := strings.LastIndexAny(seg, ".!?;:\n"); k >= 0 {
			seg = seg[k+1:] // stay within the current clause
		}
		seg = " " + seg // give a cue at the segment start a leading boundary
		negated := false
		for _, cue := range reducerNegationCues {
			if strings.Contains(seg, cue) {
				negated = true
				break
			}
		}
		if !negated {
			return true
		}
	}
}

// processTaskTitleCues are the title wordings that name a PROCESS pass rather than a
// deliverable. Matched on the title only: plenty of real tasks say "verify" in their
// done_when, and the intent of a genuine feature can mention testing without being one.
var processTaskTitleCues = []string{
	"verification", "verify", "end-to-end", "end to end", "e2e",
	"smoke test", "test pass", "testing pass", "qa pass", "qa sweep",
	"final pass", "final check", "final sweep", "regression", "polish pass",
}

// platformOwnedTaskProblem fires when a task produces nothing and its title names a
// verification/QA/polish pass. The platform already closes the build: it loads every
// page in a real browser, clicks its controls, checks endpoint shapes and auth gating,
// and sends what it finds back for a bounded repair round. A checklist item that redoes that sweep by hand costs a
// probe round-trip per page, each re-reading the whole build context, and can cost a
// large share of a build's wall clock and tokens for coverage already held.
//
// Deliberately narrow: it needs BOTH signals. A task owning no paths/edits/exports is
// common and legitimate (a stylesheet or SEO task can declare none), and a title cue
// alone would catch a real feature about testing. Only the pair is unambiguous.
func platformOwnedTaskProblem(p *Plan) string {
	var named []string
	for _, t := range p.Tasks {
		if len(t.Paths) > 0 || len(t.Edits) > 0 || len(t.Exports) > 0 {
			continue // owns something, so it is a deliverable whatever it is called
		}
		title := strings.ToLower(t.Title)
		for _, cue := range processTaskTitleCues {
			if strings.Contains(title, cue) {
				named = append(named, fmt.Sprintf("%q (%s)", t.Title, t.ID))
				break
			}
		}
	}
	if len(named) == 0 {
		return ""
	}
	sort.Strings(named)
	return fmt.Sprintf("These tasks produce nothing and re-run work the platform already does: %s. "+
		"After the last task the platform loads every page in a real browser, clicks its controls, checks endpoint shapes and auth gating, "+
		"and sends what it finds back to BUILD to fix, so a hand-run verification sweep buys no coverage and costs a "+
		"probe round-trip per page. BUILD also probes each page as it wires it. Delete these tasks. If any of them "+
		"carried real work (a screen, a module, a capability nothing else covers), re-emit it as a task that OWNS "+
		"that work in `paths`, under a title naming the thing built.", strings.Join(named, ", "))
}

// pageGateCues are the words a plan uses for a check that only the page runs: a
// passcode or PIN screen in front of a staff or admin view. Without a login surface,
// nothing on the server enforces it.
var pageGateCues = []string{
	"passcode", "pass code", "pin code", "access code", "staff code", "admin password",
	"password-protected", "password protected", "staff-only", "staff only", "admin-only", "admin only",
}

// pageOnlyGateProblem fires when a task gates a view behind a passcode-style check, the
// plan has no login surface, and the project serves data over an API: every endpoint is
// still callable without the code, so the gate hides a page but protects nothing (the
// trust-boundary law). A task that already routes the protected writes through a server
// function checking a token is left alone.
func pageOnlyGateProblem(p *Plan) string {
	if planHasAuthSurface(p) || !planNeedsBackbone(p) {
		return ""
	}
	var gated []string
	tokenChecked := false
	for _, t := range p.Tasks {
		text := taskText(t)
		for _, n := range t.Tools {
			if strings.TrimSpace(n) == "manage_function" && strings.Contains(text, "token") {
				tokenChecked = true
			}
		}
		for _, cue := range pageGateCues {
			if strings.Contains(text, cue) {
				gated = append(gated, fmt.Sprintf("%q", t.Title))
				break
			}
		}
	}
	if len(gated) == 0 || tokenChecked {
		return ""
	}
	return fmt.Sprintf("%s gate%s a view behind a code, but the plan has no login surface, so every API the project serves "+
		"stays callable by anyone who skips the page: the gate hides UI and protects nothing. Pick one: add `create_auth` "+
		"with a role and scope the protected writes to it (with a seeded account for that role), or route the protected writes "+
		"through a `manage_function` that verifies a token the gate issues and keep the table's public API read-only. "+
		"Either way the owner sets the code: ask for it as a `secret` question rather than inventing one.",
		strings.Join(gated, ", "), map[bool]string{true: "s", false: ""}[len(gated) == 1])
}

// isProjectShapeTopic reports whether a decision topic names the project's shape (site,
// app or headless). A topic that merely contains "shape" for something else ("response
// shape", "data shape") is not the project shape, and reading it as one would flip a
// headless project to having a UI or the reverse.
func isProjectShapeTopic(topic string) bool {
	t := strings.ToLower(strings.TrimSpace(topic))
	if !strings.Contains(t, "shape") {
		return false
	}
	for _, other := range []string{"response", "payload", "data", "json", "api", "envelope", "table", "row", "record", "message", "event"} {
		if strings.Contains(t, other) {
			return false
		}
	}
	return true
}
