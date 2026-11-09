package engine

// Plan-driven guide auto-prefetch: the build seed carries the capability guides a
// plan structurally implies (see planGuideSignals), so the builder writes against
// each contract with no recall round-trip. Every signal->guide map is derived from
// guide front-matter, so no routing table here names a guide, and rivals resolve to one.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"openfabrica/internal/jsonrepair"
	"openfabrica/internal/stringutil"
	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// signalClientSplit names the plan SHAPE that implies the frontend architecture
// guides: a page split into sibling client-JS modules (see planImpliesClientSplit).
// Which guides that shape needs is declared in the guides' `structural:` front-matter,
// not named here.
const signalClientSplit = "client_split"

// signalHasUI names the plan SHAPE that implies the visual craft guide: any
// project with a user-facing surface (not headless). The shape->guide mapping lives
// in the guide's `structural:` front-matter.
const signalHasUI = "has_ui"

// clientSplitEngineLibs are the rendering/engine libraries whose presence implies
// a multi-module client-JS build (and so the client_js_split guide).
var clientSplitEngineLibs = []string{"three", "phaser", "pixi", "babylon"}

// engineLibRe matches an engine lib as a whole token in a lowercased library spec:
// a left boundary blocks mid-word hits, and a forbidden trailing digit blocks
// "babylon5" while still matching "three.js" / "pixi.min.js". Derived from
// clientSplitEngineLibs so the list stays single-sourced.
var engineLibRe = buildEngineLibRe(clientSplitEngineLibs)

func buildEngineLibRe(libs []string) *regexp.Regexp {
	quoted := make([]string, len(libs))
	for i, l := range libs {
		quoted[i] = regexp.QuoteMeta(l)
	}
	return regexp.MustCompile(`(?:^|[^a-z0-9])(?:` + strings.Join(quoted, "|") + `)(?:[^0-9]|$)`)
}

// libraryTokenRes caches the whole-token pattern per library name.
var libraryTokenRes sync.Map // string -> *regexp.Regexp

// libraryNamed reports whether a lowercased library spec (module name, name and URL
// joined) names the library as a whole token: not preceded by a letter or digit and
// not followed by a letter. "three" matches "three.js", "three-stdlib" and a
// ".../three@0.160/..." URL, and not "threesome" or "withthree"; a trailing digit is
// allowed so "phaser3" still reaches the phaser guide. A plain substring test
// routed a guide for any spec that merely contained the name.
func libraryNamed(spec, lib string) bool {
	re, ok := libraryTokenRes.Load(lib)
	if !ok {
		re, _ = libraryTokenRes.LoadOrStore(lib, regexp.MustCompile(`(?:^|[^a-z0-9])`+regexp.QuoteMeta(lib)+`(?:[^a-z]|$)`))
	}
	return re.(*regexp.Regexp).MatchString(spec)
}

// guideStringMap inverts one guide front-matter field into a value -> guide-slugs
// map: every lowercased value from field(g) maps to the guide's slug, in (position,
// slug) order. Single-sourced in the guides, so no hand-maintained routing table can
// drift. Many-to-many. Deterministic.
func guideStringMap(ps []pack.Pack, field func(pack.Pack) []string) map[string][]string {
	out := map[string][]string{}
	for _, g := range pack.Capabilities(ps) {
		for _, v := range field(g) {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				out[v] = append(out[v], g.Slug)
			}
		}
	}
	return out
}

// guideToolMap maps tool name -> guide slugs from each guide's `tools:` front-matter.
func guideToolMap(ps []pack.Pack) map[string][]string {
	return guideStringMap(ps, func(g pack.Pack) []string { return g.Tools })
}

// guideStructuralMap maps structural signal -> guide slugs from `structural:` front-matter.
func guideStructuralMap(ps []pack.Pack) map[string][]string {
	return guideStringMap(ps, func(g pack.Pack) []string { return g.Structural })
}

// guideLibraryMap maps library substring -> guide slugs from `libraries:` front-matter.
func guideLibraryMap(ps []pack.Pack) map[string][]string {
	return guideStringMap(ps, func(g pack.Pack) []string { return g.Libraries })
}

// planImpliesClientSplit reports whether the plan implies a multi-module client
// JS build: a page split into subsystem leaf modules (a task owning more than one
// /files JS, or one that EDITS a sibling's JS entry), or a declared engine library.
// Those are the builds where the import-tree / cyclic-import rules bite. Deterministic.
func planImpliesClientSplit(plan *Plan) bool {
	if plan == nil {
		return false
	}
	for _, l := range plan.ExternalLibraries {
		spec := strings.ToLower(l.ModuleName + " " + l.Name + " " + l.URL)
		if engineLibRe.MatchString(spec) {
			return true
		}
	}
	isJS := func(p string) bool {
		return strings.HasSuffix(strings.ToLower(strings.TrimSpace(p)), ".js")
	}
	for _, t := range plan.Tasks {
		jsOwned := 0
		for _, p := range t.Paths {
			if isJS(p) {
				jsOwned++
			}
		}
		if jsOwned > 1 {
			return true
		}
		for _, e := range t.Edits {
			if isJS(e) {
				return true
			}
		}
	}
	// A page expressed over its ROUTE - a spine route that >= subsystemLeafThreshold
	// tasks EDIT while owning no leaf of their own - is a multi-module client build too,
	// even with no planner-named .js file, so inject the import-tree guide for it.
	if _, _, ok := fanoutEntry(plan, subsystemLeafThreshold); ok {
		return true
	}
	return false
}

// planHasUI reports whether the plan builds a user-facing surface: any shape
// other than headless. The project-shape decision is authoritative; when a plan
// omits it, a page-owning task still marks a UI build. Deterministic.
func planHasUI(plan *Plan) bool {
	if plan == nil {
		return false
	}
	for _, d := range plan.Decisions {
		if !isProjectShapeTopic(d.Topic) {
			continue
		}
		return !strings.Contains(strings.ToLower(strings.TrimSpace(d.Choice)), "headless")
	}
	return planHasPageTask(plan)
}

// planHasPageTask reports whether any task owns a page surface: a route path or
// the shared layout. Table and endpoint tasks own bare resource names, and split
// leaves own /files modules, so neither counts as a page. Deterministic.
func planHasPageTask(plan *Plan) bool {
	for _, t := range plan.Tasks {
		for _, p := range t.Paths {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "default" || isRootedNonAsset(p) {
				return true
			}
		}
	}
	return false
}

// planClientJSFiles counts the DISTINCT project client-JS files a plan touches
// across every task (owned Paths + edited Edits), deduped case-insensitively. Two
// or more means the build ships sibling JS modules that may need to share a symbol.
// Deterministic.
func planClientJSFiles(plan *Plan) int {
	if plan == nil {
		return 0
	}
	isJS := func(p string) bool {
		return strings.HasSuffix(strings.ToLower(strings.TrimSpace(p)), ".js")
	}
	seen := map[string]bool{}
	for _, t := range plan.Tasks {
		for _, p := range append(append([]string{}, t.Paths...), t.Edits...) {
			if isJS(p) {
				seen[strings.ToLower(strings.TrimSpace(p))] = true
			}
		}
	}
	return len(seen)
}

// planNeedsWindowBridgeRule reports whether the intent seed should carry the compact
// window-bridge rule: the plan ships two or more client-JS files but does NOT pull the
// full client_js_split guide (which covers it in depth). Asks planImpliesClientSplit
// rather than the client_split tag: only the import-tree guide covers the window bridge.
func planNeedsWindowBridgeRule(plan *Plan, ps []pack.Pack) bool {
	if planImpliesClientSplit(plan) {
		return false // the full split guide already carries the window-bridge rule
	}
	return planClientJSFiles(plan) >= 2
}

// guideSignal ranks HOW a guide was reached, for resolving two selected RIVAL
// architectures to the stronger signal (see resolveExclusives): a locked decision
// outranks an inferred signal outranks a bare named slug outranks a keyword hit.
type guideSignal int

const (
	signalKeyword  guideSignal = iota // task-text keyword inference alone implied it
	signalNamed                       // plan.Guides, or the PLAN picker's reply
	signalInferred                    // a declared tool, library or structural shape implied it
	signalLocked                      // a locked plan decision named it
)

// planGuideSlugs returns the set of guide slugs a plan implies, from its
// structured signals, with rival-architecture guides resolved to one.
// Deterministic.
//
// An evidence-gated guide (large core, easy to false-positive) is dropped when
// its ONLY signal is keyword inference: a phrase like "support chat" must not
// inline a 20KB+ realtime core into every build turn. The filter runs BEFORE
// base closure so a dropped guide cannot drag its required bases in. The tool
// preload itself is untouched (one schema is cheap, see buildPreloadTools).
func planGuideSlugs(plan *Plan, ps []pack.Pack) map[string]bool {
	ranked := planGuideSignals(plan, ps)
	for _, g := range pack.Capabilities(ps) {
		if !g.EvidenceGated {
			continue
		}
		// Endpoint guides carry a short, always-relevant route contract (the
		// /api/<path>/upload shape a page must call), so keep them even on a
		// keyword-only signal. The false-positive concern is the large realtime
		// and rendering cores, not these.
		if g.Category == "endpoints" {
			continue
		}
		if sig, seen := ranked[g.Slug]; seen && sig == signalKeyword {
			delete(ranked, g.Slug)
		}
	}
	kept, _ := resolveExclusives(ranked, ps, requiredBases(ranked, ps))
	return filterRequiresAnyOf(withRequiredBases(kept, ps), ps, lockedDecisionTopics(plan), false)
}

// requiredBases returns the guide slugs that are a `requires` base of any wanted
// guide, closed transitively. A base pulled in by a wanted guide must not be dropped
// in rival resolution: resolution runs before the requires closure, so a dropped base
// would be re-admitted afterwards and its rival would ride back in alongside it (a
// heightfield plan that also names three + phaser would otherwise get both rendering
// cores). Passed to resolveExclusives as the protected set.
func requiredBases(want map[string]guideSignal, ps []pack.Pack) map[string]bool {
	valid := validCapabilitySlugs(ps)
	inScope := make(map[string]bool, len(want))
	for slug := range want {
		inScope[slug] = true
	}
	out := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, g := range pack.Capabilities(ps) {
			if !inScope[g.Slug] {
				continue
			}
			for _, req := range g.Requires {
				if req = strings.TrimSpace(req); valid[req] && !out[req] {
					out[req] = true
					inScope[req] = true
					changed = true
				}
			}
		}
	}
	return out
}

// decisionAnchorPrefix marks a `requires_any_of` anchor that is a locked plan decision
// rather than a guide slug: `decision:<topic>/<choice words>` is satisfied by a locked
// decision with that topic whose choice contains every one of those words (the same
// token-subset rule as matchDecisionAlias). It lets an add-on ride a choice no guide
// backs - a vanilla-canvas game locks its rendering approach but selects no engine guide.
const decisionAnchorPrefix = "decision:"

// lockedDecisionTopics returns each locked decision's normalized topic mapped to its
// choice tokens, for decision anchors in filterRequiresAnyOf. Nil for a nil plan.
func lockedDecisionTopics(plan *Plan) map[string]map[string]bool {
	if plan == nil || len(plan.Decisions) == 0 {
		return nil
	}
	out := map[string]map[string]bool{}
	for _, d := range plan.Decisions {
		t := normDecision(d.Topic)
		toks := decisionTokens(d.Choice)
		if t == "" || len(toks) == 0 {
			continue
		}
		if out[t] == nil {
			out[t] = map[string]bool{}
		}
		for _, tok := range toks {
			out[t][tok] = true
		}
	}
	return out
}

// decisionAnchorMet reports whether a `decision:<topic>/<choice words>` anchor is
// satisfied by the locked decisions. While decisions are still open (PLAN, before
// the planner locks them) a topic nobody has decided yet counts as met, since the
// guide's own decision block is what gets it decided.
func decisionAnchorMet(anchor string, locked map[string]map[string]bool, open bool) bool {
	topic, choice, _ := strings.Cut(anchor, "/")
	have, ok := locked[normDecision(topic)]
	if !ok {
		return open
	}
	return tokensSubset(decisionTokens(choice), have)
}

// filterRequiresAnyOf drops an ADD-ON guide (one declaring `requires_any_of`) whose
// anchor list does not intersect the selected set: game_feel layers juice on a
// rendering guide, so it must not ride into a build that selected no game/canvas
// surface. An anchor may also be a locked decision topic (decisionAnchorPrefix), so a
// game that commits to vanilla canvas keeps its add-on. open marks the PLAN pass, where
// an undecided topic still counts as met. Iterates to a fixed point so
// dropping one add-on can drop another that hung off it. (This is the deterministic
// gate; whether a 3D scene is an interactive game vs a static render is a call only
// the planner can make.)
func filterRequiresAnyOf(want map[string]bool, ps []pack.Pack, locked map[string]map[string]bool, open bool) map[string]bool {
	for changed := true; changed; {
		changed = false
		for _, g := range pack.Capabilities(ps) {
			if !want[g.Slug] || len(g.RequiresAnyOf) == 0 {
				continue
			}
			ok := false
			for _, anchor := range g.RequiresAnyOf {
				anchor = strings.TrimSpace(anchor)
				if spec, isDecision := strings.CutPrefix(anchor, decisionAnchorPrefix); isDecision {
					ok = decisionAnchorMet(spec, locked, open)
				} else {
					ok = want[anchor]
				}
				if ok {
					break
				}
			}
			if !ok {
				delete(want, g.Slug)
				changed = true
			}
		}
	}
	return want
}

// planGuideSignals returns the guide slugs a plan implies mapped to the STRONGEST
// signal that reached each one, validated against the real capability-guide set in
// ps (an unknown/stale slug is dropped). Deterministic.
func planGuideSignals(plan *Plan, ps []pack.Pack) map[string]guideSignal {
	if plan == nil {
		return nil
	}
	valid := validCapabilitySlugs(ps)
	out := map[string]guideSignal{}
	addAs := func(slug string, sig guideSignal) {
		slug = strings.TrimSpace(slug)
		if slug == "" || !valid[slug] {
			return
		}
		if prev, seen := out[slug]; !seen || sig > prev {
			out[slug] = sig
		}
	}
	add := func(slug string) { addAs(slug, signalInferred) }
	// 1) every tool the build will load, mapped via guide metadata, ranked by
	//    HOW the tool was reached: a planner-declared task tool is real intent
	//    (signalInferred), a task-text keyword hit is only a hunch
	//    (signalKeyword, which planGuideSlugs can veto for evidence-gated
	//    guides). Core and backbone tools are never in any guide's `tools:`,
	//    so skipping buildPreloadTools here loses nothing; the tool preload
	//    itself still unions everything (see buildAllTools).
	toolGuide := guideToolMap(ps)
	guidesForTool := func(name string, sig guideSignal) {
		for _, slug := range toolGuide[strings.ToLower(strings.TrimSpace(name))] {
			addAs(slug, sig)
		}
	}
	// A keyword hit a locked decision rules out ("no-auth" matching auth) pulls no
	// guide, mirroring buildAllTools' preload veto.
	vetoed := decisionVetoedTools(plan)
	for _, t := range plan.Tasks {
		for _, name := range t.Tools {
			guidesForTool(name, signalInferred)
		}
		for _, name := range inferTaskTools(Task{Title: t.Title, Intent: t.Intent}) {
			if vetoed[name] {
				continue
			}
			guidesForTool(name, signalKeyword)
		}
	}
	// 2) declared external libraries.
	libGuide := guideLibraryMap(ps)
	for _, l := range plan.ExternalLibraries {
		spec := strings.ToLower(l.ModuleName + " " + l.Name + " " + l.URL)
		for sub, slugs := range libGuide {
			if !libraryNamed(spec, sub) {
				continue
			}
			for _, slug := range slugs {
				add(slug)
			}
		}
	}
	// 3) structural: a multi-module / engine-library plan implies the frontend
	//    architecture guides via their `structural:` front-matter.
	if planImpliesClientSplit(plan) {
		for _, slug := range guideStructuralMap(ps)[signalClientSplit] {
			add(slug)
		}
	}
	//    Any UI-bearing plan (a page or canvas, so anything not headless) pulls the
	//    visual craft guide the same way, via its `structural:` front-matter.
	if planHasUI(plan) {
		for _, slug := range guideStructuralMap(ps)[signalHasUI] {
			add(slug)
		}
	}
	//    The user_flows guide rides the engine's own checklist item rather than a
	//    structural tag: the item exists only when the owner turned flow tests on and
	//    the plan builds pages or an API, which is exactly when the guide is owed
	//    (a headless API is not has_ui, and with tests off nothing writes flows).
	for _, t := range plan.Tasks {
		if stringutil.TaskSlug(t.ID) == userFlowsTaskID {
			add("user_flows")
			break
		}
	}
	// 4) locked decisions. A decision naming a capability is a stronger signal than
	//    the planner remembering to repeat the slug in plan.Guides, and these guides
	//    have no tool or library to infer from.
	decGuide := guideDecisionMap(ps)
	var decAliases []decisionAlias // built lazily, only when an exact lookup misses
	for _, dec := range plan.Decisions {
		if slug := decGuide[decisionKey(dec.Topic, dec.Choice)]; slug != "" {
			addAs(slug, signalLocked)
			continue
		}
		// Exact miss: fall back to token-subset matching so a locked choice that adds
		// or reorders words ("heightfield terrain" for "heightfield") still routes its
		// guide instead of silently building with no contract.
		if decAliases == nil {
			decAliases = guideDecisionAliases(ps)
		}
		addAs(matchDecisionAlias(decAliases, dec.Topic, dec.Choice), signalLocked)
	}
	// 5) guides the planner explicitly declared (the channel for capabilities no
	//    tool/library/structural/decision signal can express, e.g. e2ee; webrtc_signaling now rides manage_signaling).
	for _, slug := range plan.Guides {
		if canon, ok := canonicalGuideSlug(slug, valid); ok {
			slug = canon
		}
		addAs(slug, signalNamed)
	}
	return out
}

// guideDecisionMap returns the locked `topic/choice` -> guide-slug map DERIVED from
// the guides' own `decisions:` front-matter. Only a guide with no tool, library or
// structural signal needs one. Keys run through decisionKey so wording drift in the
// recorded choice (case, spacing) does not silently stop matching. Deterministic.
func guideDecisionMap(ps []pack.Pack) map[string]string {
	out := map[string]string{}
	for _, g := range pack.Capabilities(ps) {
		for _, d := range g.Decisions {
			topic, choice, ok := strings.Cut(d, "/")
			if !ok {
				continue
			}
			if key := decisionKey(topic, choice); key != "/" {
				out[key] = g.Slug
			}
		}
	}
	return out
}

// normDecision normalizes one decision topic or choice: lowercase, hyphens and
// underscores folded to spaces, whitespace runs collapsed.
func normDecision(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("-", " ", "_", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// decisionKey normalizes a decision's topic and choice into the exact map's key
// shape (see guideDecisionMap).
func decisionKey(topic, choice string) string {
	return normDecision(topic) + "/" + normDecision(choice)
}

// decisionTokens splits a normalized topic or choice into its words, treating any
// punctuation as a separator so "vanilla <canvas> + rAF" yields the word "canvas".
// Only the tolerant matchers use tokens; exact keys (decisionKey) are unchanged.
func decisionTokens(s string) []string {
	return strings.FieldsFunc(normDecision(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// decisionAlias is one guide's `<topic>/<choice>` alias, pre-normalized for tolerant
// (token-subset) matching when a locked choice misses the exact map.
type decisionAlias struct {
	topic  string   // normalized topic
	tokens []string // normalized choice words
	slug   string
}

// guideDecisionAliases flattens every guide's `decisions:` front-matter into
// normalized aliases, for the token-subset fallback in planGuideSignals.
func guideDecisionAliases(ps []pack.Pack) []decisionAlias {
	var out []decisionAlias
	for _, g := range pack.Capabilities(ps) {
		for _, d := range g.Decisions {
			topic, choice, ok := strings.Cut(d, "/")
			if !ok {
				continue
			}
			nt := normDecision(topic)
			toks := decisionTokens(choice)
			if nt == "" || len(toks) == 0 {
				continue
			}
			out = append(out, decisionAlias{topic: nt, tokens: toks, slug: g.Slug})
		}
	}
	return out
}

// matchDecisionAlias routes a locked decision that missed the exact map by wording
// drift beyond case and spacing (extra or reordered words, e.g. "heightfield terrain"
// for the "heightfield" alias). It matches an alias whose every token appears in the
// locked choice, under the same topic, and returns a slug only when exactly one guide
// matches, so the 1:1 decision -> guide guarantee (TestGuideDecisionsAreUnambiguous) is
// preserved. Empty on no match or an ambiguous one.
func matchDecisionAlias(aliases []decisionAlias, topic, choice string) string {
	nt := normDecision(topic)
	have := map[string]bool{}
	for _, t := range decisionTokens(choice) {
		have[t] = true
	}
	if nt == "" || len(have) == 0 {
		return ""
	}
	winner := ""
	for _, a := range aliases {
		if a.topic != nt || !tokensSubset(a.tokens, have) {
			continue
		}
		if winner != "" && winner != a.slug {
			return "" // two guides claim this choice - refuse rather than guess
		}
		winner = a.slug
	}
	return winner
}

// tokensSubset reports whether every token in need is present in have.
func tokensSubset(need []string, have map[string]bool) bool {
	for _, t := range need {
		if !have[t] {
			return false
		}
	}
	return true
}

// guideExclusiveMap returns the slug -> rival-slugs map DERIVED from the guides'
// `exclusive_with:` front-matter, symmetrized so declaring the rivalry on either
// member is enough. Deterministic.
func guideExclusiveMap(ps []pack.Pack) map[string]map[string]bool {
	valid := validCapabilitySlugs(ps)
	out := map[string]map[string]bool{}
	link := func(a, b string) {
		if out[a] == nil {
			out[a] = map[string]bool{}
		}
		out[a][b] = true
	}
	for _, g := range pack.Capabilities(ps) {
		for _, rival := range g.ExclusiveWith {
			rival = strings.TrimSpace(rival)
			if rival == "" || rival == g.Slug || !valid[rival] {
				continue
			}
			link(g.Slug, rival)
			link(rival, g.Slug)
		}
	}
	return out
}

// resolveExclusives keeps at most one guide from each rival-architecture group
// (additive guides only cost tokens, but two rival architectures derail the
// build). A total order picks the winner regardless of map iteration:
//  0. a protected guide (a required base of a wanted guide) beats an unprotected
//     rival, so the requires closure cannot re-admit a rival dropped here;
//  1. stronger signal (locked decision > inferred > bare named slug);
//  2. `exclusive_default: true`: the safer member on a tie;
//  3. slug order, as a deterministic backstop.
//
// Returns the surviving set plus the dropped slugs. Deterministic.
func resolveExclusives(ranked map[string]guideSignal, ps []pack.Pack, protected map[string]bool) (map[string]bool, []string) {
	if len(ranked) == 0 {
		return map[string]bool{}, nil
	}
	excl := guideExclusiveMap(ps)
	def := map[string]bool{}
	for _, g := range pack.Capabilities(ps) {
		if g.ExclusiveDefault {
			def[g.Slug] = true
		}
	}
	// beats reports whether a outranks b. Total order, so it never reports both.
	beats := func(a, b string) bool {
		if protected[a] != protected[b] {
			return protected[a]
		}
		if ranked[a] != ranked[b] {
			return ranked[a] > ranked[b]
		}
		if def[a] != def[b] {
			return def[a]
		}
		return a < b
	}
	out := make(map[string]bool, len(ranked))
	var dropped []string
	for _, g := range pack.Capabilities(ps) { // deterministic order
		if _, want := ranked[g.Slug]; !want {
			continue
		}
		beaten := false
		for rival := range excl[g.Slug] {
			_, alsoWanted := ranked[rival]
			// A protected base is not in ranked yet but the requires closure re-admits
			// it after resolution, so count it as present here. Otherwise a guide whose
			// only rival is that base survives and both cores get fed.
			if (alsoWanted || protected[rival]) && beats(rival, g.Slug) {
				beaten = true
				break
			}
		}
		if beaten {
			dropped = append(dropped, g.Slug)
			continue
		}
		out[g.Slug] = true
	}
	return out, dropped
}

// validCapabilitySlugs returns the set of real capability-guide slugs in ps.
func validCapabilitySlugs(ps []pack.Pack) map[string]bool {
	out := map[string]bool{}
	for _, g := range pack.Capabilities(ps) {
		out[g.Slug] = true
	}
	return out
}

// renderGuidesFor renders the CORE (contracts transcluded) plus a recall index
// of the wanted guides, in (position, slug) order. Empty when none. locked
// (topic -> choice, lowercased) trims a locked Decision block's unchosen
// branches; nil renders everything.
func renderGuidesFor(want map[string]bool, ps []pack.Pack, locked map[string]string) string {
	if len(want) == 0 {
		return ""
	}
	guides := orderedGuides(want, ps)
	if len(guides) == 0 {
		return ""
	}
	return pack.RenderGuidesCoreLocked(guides, locked)
}

// guideCapabilityGaps checks the wanted guides' declared tools against the configured
// provider capabilities so the build seed can warn up front instead of letting the
// builder hit a refused find_tool load. Returns the wanted guides minus any fully dead
// one (declares tools and every one is gated off), plus one note per affected guide.
// Fail-open on a nil registry or unknown tool. Deterministic.
func guideCapabilityGaps(want map[string]bool, ps []pack.Pack, reg *tools.Registry, caps llm.Capabilities) (map[string]bool, []string) {
	if reg == nil || len(want) == 0 {
		return want, nil
	}
	keep := make(map[string]bool, len(want))
	var notes []string
	for _, g := range pack.Capabilities(ps) {
		if !want[g.Slug] {
			continue
		}
		alive, dead := guideToolLiveness(g, reg, caps)
		if !alive {
			notes = append(notes, fmt.Sprintf("The %s guide was skipped: every tool it covers is unavailable in this configuration - %s. Do not build that capability and do not search for its tools; %s",
				g.Slug, strings.Join(dead, ", "), guideGapFallback(g, reg)))
			continue
		}
		keep[g.Slug] = true
		if len(dead) > 0 {
			notes = append(notes, fmt.Sprintf("%s guide: %s - unavailable in this configuration, and find_tool will refuse to load them. Use the guide's other tools or leave that part out; do not retry the load.",
				g.Slug, strings.Join(dead, ", ")))
		}
	}
	return keep, notes
}

// guideToolLiveness reports whether any tool a guide declares is callable here,
// and names the ones that are not. A guide declaring no tools is always alive.
// Fail-open on an unknown name, so a stale front-matter entry cannot hide a
// working guide.
func guideToolLiveness(g pack.Pack, reg *tools.Registry, caps llm.Capabilities) (alive bool, dead []string) {
	return reg.AnyCallable(g.Tools, caps)
}

// guideGapFallback returns what to do instead of a guide whose tools are all
// gated off, taken from the registry's per-capability guidance so the advice has
// one home. Falls back to a generic line when the guide names no known tool.
func guideGapFallback(g pack.Pack, reg *tools.Registry) string {
	for _, name := range g.Tools {
		def, err := reg.Get(strings.ToLower(strings.TrimSpace(name)))
		if err != nil {
			continue
		}
		if guidance := tools.CapabilityFallback(def.RequiresCapability); guidance != "" {
			return guidance
		}
	}
	return "build the project without it."
}

// capabilityDeadGuides is the set of guide slugs whose every declared tool is
// gated off here. The guide index leaves them out: naming a capability whose
// tools cannot be loaded reads as an invitation to go find them, and the search
// costs a turn and a full context re-read each round.
func capabilityDeadGuides(ps []pack.Pack, reg *tools.Registry, caps llm.Capabilities) map[string]bool {
	if reg == nil {
		return nil
	}
	out := map[string]bool{}
	for _, g := range pack.Capabilities(ps) {
		if alive, _ := guideToolLiveness(g, reg, caps); !alive {
			out[g.Slug] = true
		}
	}
	return out
}

// withoutDeadGuides drops the capability guides whose every tool is gated off, so a
// surface that selects from the pack list (the PLAN picker and its fallback) never
// offers a capability no tool can build.
func withoutDeadGuides(ps []pack.Pack, reg *tools.Registry, caps llm.Capabilities) []pack.Pack {
	dead := capabilityDeadGuides(ps, reg, caps)
	if len(dead) == 0 {
		return ps
	}
	out := make([]pack.Pack, 0, len(ps))
	for _, p := range ps {
		if p.Role == pack.RoleCapability && dead[p.Slug] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// --- PLAN-time guide selection (LLM-picked) --------------------------------
//
// PLAN has no plan yet, only the brief, so a small structured LLM pre-pass picks which
// capability guides the planner needs, rendered as their DECISION facet. The picker's
// per-guide rules all live in guide front-matter (use_when/not_when), not the prompt.
// Any miss falls back to every guide's decision facet (unroutedGuideFallback); rivals
// resolve to one. The pre-pass rides the session loop, so a crash resume replays the pick.

// guideSelectionSchema constrains the pre-pass reply to {"guides":[slug,...]}.
func guideSelectionSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Name:   "guide_selection",
		Strict: false,
		Schema: json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["guides"],
  "properties": {
    "guides": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Slugs of the capability guides this project needs, from the provided list. Empty when the project is a plain content site."
    }
  }
}`),
	}
}

// guideSelectionIndex renders the picker's catalogue: one line per capability guide
// (slug + summary), plus that guide's own `use_when:` / `not_when:` prose and an
// `(needs explicit evidence)` marker for an `evidence_gated:` guide. Every per-guide
// rule comes from front-matter, so adding a guide never edits the prompt. Deterministic.
func guideSelectionIndex(caps []pack.Pack) string {
	excl := guideExclusiveMap(caps)
	var b strings.Builder
	for _, g := range caps {
		summary := g.Summary
		if summary == "" {
			summary = g.Title
		}
		var marks []string
		if g.EvidenceGated {
			marks = append(marks, "needs explicit evidence")
		}
		if rivals := sortedSlugs(excl[g.Slug]); len(rivals) > 0 {
			marks = append(marks, "pick at most one of: "+strings.Join(rivals, ", "))
		}
		var gate string
		if len(marks) > 0 {
			gate = " (" + strings.Join(marks, "; ") + ")"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", g.Slug, gate, summary)
		if w := strings.TrimSpace(g.UseWhen); w != "" {
			fmt.Fprintf(&b, "    Use when: %s\n", w)
		}
		if n := strings.TrimSpace(g.NotWhen); n != "" {
			fmt.Fprintf(&b, "    Not for: %s\n", n)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// guideSelectionSystem is the picker's system prompt: choose the guides a brief
// implies. It carries only the GENERIC selection rules; every per-guide rule lives in
// guide front-matter and reaches the picker via guideSelectionIndex, so this prompt
// does not grow with the corpus.
func guideSelectionSystem(caps []pack.Pack) string {
	return "You are a routing step before project planning. Given a project brief, pick which platform capability guides the planner must read to design the project correctly (it sees each picked guide's architecture decisions; the build later gets each guide's core). Each guide covers one capability (realtime, payments, encryption, 3D, AI, automation, ...). The brief may be written in any language: judge it by meaning, not by English wording, and apply the evidence rules below the same way in every language.\n\n" +
		"Return the slugs of every guide the brief implies. When a capability is plausible but not certain, INCLUDE it: a missing guide makes the planner miss a locked architecture decision, while an extra one only costs tokens. Include a guide's base when it depends on one (a WebRTC call needs realtime; a voxel world needs 3D). A plain content site (blog, portfolio, landing page, brochure, marketing site) needs NONE - return an empty list.\n\n" +
		"The include-when-unsure rule is for ADDITIVE capabilities that sit alongside each other harmlessly. Two markers in the catalogue below override it:\n" +
		"- `(needs explicit evidence)` - a guide with a clear positive signal and a large body, where include-when-unsure would just tax every plan with thousands of wasted tokens. Include one of those ONLY on explicit evidence in the brief, not merely because it is plausible. Its `Use when` line says what counts as evidence and its `Not for` line says what does not; read both before deciding.\n" +
		"- `(pick at most one of: ...)` - guides that lock RIVAL, mutually-exclusive architectures for the same thing. There an extra guide does not just cost tokens, it derails the build toward the wrong architecture. Pick AT MOST ONE, on explicit evidence, reading each rival's `Use when` and `Not for` lines to tell them apart. If you return more than one, only the best-evidenced survives.\n\n" +
		"Available guides (slug: what it covers, then when to reach for it):\n" + guideSelectionIndex(caps) + "\n\n" +
		"Return only the JSON object with the `guides` array of slugs."
}

// parseGuideSelection extracts the valid guide slugs from the picker's JSON reply.
// Unknown slugs are dropped; a parse failure returns an error so the caller can
// fall back to the full catalogue (unroutedGuideFallback).
func parseGuideSelection(content string, valid map[string]bool) ([]string, error) {
	var out struct {
		Guides []string `json:"guides"`
	}
	if err := json.Unmarshal([]byte(jsonrepair.Repair(content)), &out); err != nil {
		return nil, err
	}
	var slugs []string
	for _, s := range out.Guides {
		if canon, ok := canonicalGuideSlug(s, valid); ok {
			slugs = append(slugs, canon)
		}
	}
	return slugs, nil
}

// canonicalGuideSlug resolves a model-written slug to a real one, tolerating
// whitespace, case, and hyphens for underscores. False when nothing matches.
func canonicalGuideSlug(s string, valid map[string]bool) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if valid[s] {
		return s, true
	}
	norm := strings.ToLower(strings.ReplaceAll(s, "-", "_"))
	if valid[norm] {
		return norm, true
	}
	return "", false
}

// withRequiredBases closes want over each guide's `requires:` metadata, so selecting
// a capability always pulls the base it builds on (e2ee -> realtime_signaling). Makes
// deterministic what the picker is only prompted to do. Follows chains, though no
// guide currently chains past one level.
func withRequiredBases(want map[string]bool, ps []pack.Pack) map[string]bool {
	valid := validCapabilitySlugs(ps)
	for changed := true; changed; {
		changed = false
		for _, g := range pack.Capabilities(ps) {
			if !want[g.Slug] {
				continue
			}
			for _, req := range g.Requires {
				if req = strings.TrimSpace(req); valid[req] && !want[req] {
					want[req] = true
					changed = true
				}
			}
		}
	}
	return want
}

// orderedGuides returns the wanted guides in (position, slug) order.
func orderedGuides(want map[string]bool, ps []pack.Pack) []pack.Pack {
	var out []pack.Pack
	for _, g := range pack.Capabilities(ps) { // Capabilities preserves (position, slug) order
		if want[g.Slug] {
			out = append(out, g)
		}
	}
	return out
}

// selectPlanGuideSlugs runs the LLM pre-pass to pick the guides a brief implies,
// unioned with the existing plan's structured signals (change requests), rivals
// resolved to one. Any picker miss (empty brief, no structured-output support, a
// call failure, an unparseable reply) falls back to every guide's decision facet
// (unroutedGuideFallback). The bool reports whether the picker actually ran.
func (w *Worker) selectPlanGuideSlugs(ctx context.Context, brief string, existing *Plan, ps []pack.Pack) (map[string]bool, bool) {
	if strings.TrimSpace(brief) == "" {
		return unroutedGuideFallback(existing, ps), false // nothing to route from
	}
	caps := pack.Capabilities(ps)
	spec := SessionSpec{
		Stage:          StagePlan,
		System:         guideSelectionSystem(caps),
		Seed:           []llm.Message{{Role: llm.RoleUser, Content: "Project brief:\n\n" + strings.TrimSpace(brief) + "\n\nReturn the slugs of the guides this project needs."}},
		ResponseSchema: guideSelectionSchema(),
		MaxIterations:  1,
		MaxTokens:      512,
		Temperature:    f64(0),
		Timeout:        60 * time.Second,
		Thinking:       &thinkingOff,
		Policy:         policyPlan,
	}
	sess := w.newStageSession(StagePlan)
	rp, err := sess.resolver.resolve()
	if err != nil {
		return unroutedGuideFallback(existing, ps), false
	}
	if !supportsStructuredOutput(rp) {
		// The picker can NEVER run on this provider (a permanent property), and these
		// are exactly the small local models whose context every guide body would drown.
		return unroutedGuideFallback(existing, ps), false
	}
	res, err := w.runSpec(ctx, sess, spec)
	if err != nil {
		w.logger.Warn("plan guide selection call failed, falling back to the full guide catalogue", "error", err)
		return unroutedGuideFallback(existing, ps), false
	}
	slugs, perr := parseGuideSelection(res.Content, validCapabilitySlugs(ps))
	if perr != nil {
		w.logger.Warn("plan guide selection reply unparseable, falling back to the full guide catalogue", "error", perr)
		return unroutedGuideFallback(existing, ps), false
	}
	want := map[string]guideSignal{}
	for _, s := range slugs {
		want[s] = signalNamed // the picker volunteered it, same weight as a named slug
	}
	// Change-request union at the signal strength the EXISTING plan reached each guide
	// with, so a capability a prior update locked in outranks a picker guess at the rival.
	for slug, sig := range planGuideSignals(existing, ps) {
		if prev, seen := want[slug]; !seen || sig > prev {
			want[slug] = sig
		}
	}
	// Resolve rivals AFTER the union: two resolved sets unioned can still re-admit both.
	// Bases close over the SURVIVORS, so a dropped rival never drags its own base in.
	kept, dropped := resolveExclusives(want, ps, requiredBases(want, ps))
	if len(dropped) > 0 {
		w.logger.Warn("dropped rival architecture guides from the plan selection", "dropped", dropped, "kept", sortedSlugs(kept))
	}
	return filterRequiresAnyOf(withRequiredBases(kept, ps), ps, lockedDecisionTopics(existing), true), true
}

// sortedSlugs returns a set's members sorted, for a stable log line.
func sortedSlugs(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unroutedGuideFallback is the deterministic stand-in when the LLM picker cannot
// answer: offer the planner EVERY capability's DECISION facet, unioned with the
// existing plan's signals. The facets are cheap (~9 KB vs ~120 KB of cores). Rivals
// are NOT resolved here: with no picker reply there is no evidence, so both facets
// are shown and the planner chooses.
func unroutedGuideFallback(existing *Plan, ps []pack.Pack) map[string]bool {
	want := validCapabilitySlugs(ps)
	for slug := range planGuideSlugs(existing, ps) { // change-request union
		want[slug] = true
	}
	return withRequiredBases(want, ps)
}

// renderPlanGuides picks the guides a plan needs (LLM pre-pass + existing-plan union)
// and renders their DECISION facet for the {{GUIDES_CATALOGUE}} sentinel: the
// `## Decision:` / `## Choose:` blocks the planner locks. PLAN emits JSON and carries
// no tools, so it gets the decision facet, not the full core BUILD inlines.
func (w *Worker) renderPlanGuides(ctx context.Context, brief string, existing *Plan, ps []pack.Pack) string {
	want, _ := w.selectPlanGuideSlugs(ctx, brief, existing, ps)
	return pack.RenderGuidesDecision(orderedGuides(want, ps))
}
