package engine

import (
	"strings"
	"testing"

	"openfabrica/pack"
	"openfabrica/providers/llm"
	"openfabrica/tools"
)

// TestPlanGuideSlugs_StructuredSignals: prefetch reads the plan's STRUCTURED
// intent - the build's resolved tool set (mapped via each guide's `tools:`
// front-matter), the declared libraries, and the planner's explicit guides list
// - not fuzzy prose. Runs against the real embedded guide metadata.
func TestPlanGuideSlugs_StructuredSignals(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	voxel := &Plan{
		Goal:              "A multiplayer voxel sandbox in a shared 3D world.",
		ExternalLibraries: []LibrarySpec{{Name: "three.js", Kind: "module", ModuleName: "three", URL: "https://cdn/three.module.js"}},
		Tasks: []PlanTaskSpec{
			{Title: "Realtime tick", Tools: []string{"manage_tick"}},
			{Title: "Game page", Tools: nil},
		},
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "tick"}},
	}
	got := planGuideSlugs(voxel, ps)
	for _, want := range []string{"3d_rendering", "realtime_signaling"} {
		if !got[want] {
			t.Errorf("voxel plan should imply %q via tools/libraries; got %v", want, keys(got))
		}
	}
	for _, no := range []string{"auth_contract", "e2ee", "webrtc_signaling"} {
		if got[no] {
			t.Errorf("voxel plan should NOT imply %q; got %v", no, keys(got))
		}
	}

	// Auth and payments via DECLARED TOOLS only (no plan.Guides) - the metadata
	// map must deliver them. Plus a planner-declared guide for webrtc (the one
	// capability with no tool/library signal).
	callApp := &Plan{
		Tasks: []PlanTaskSpec{
			{Title: "Login", Tools: []string{"manage_auth"}},
			{Title: "Checkout", Tools: []string{"manage_payments"}},
			{Title: "Call", Tools: []string{"manage_signaling"}},
		},
		Guides: []string{"webrtc_signaling"},
	}
	cg := planGuideSlugs(callApp, ps)
	for _, want := range []string{"auth_contract", "payments", "realtime_signaling", "webrtc_signaling"} {
		if !cg[want] {
			t.Errorf("call app should imply %q; got %v", want, keys(cg))
		}
	}

	// A plain CRUD blog lists only CORE tools (never in any guide's `tools:`) and
	// no libs -> no prefetch (the REST envelope rides the always-on laws).
	blog := &Plan{Goal: "A blog with posts.", Tasks: []PlanTaskSpec{{Title: "Posts API"}}}
	if bg := planGuideSlugs(blog, ps); len(bg) != 0 {
		t.Errorf("plain CRUD blog should imply no prefetched guide; got %v", keys(bg))
	}

	// An unknown planner-declared slug is dropped; a real one is kept.
	bogus := &Plan{Guides: []string{"not_a_real_guide", "e2ee"}}
	bg := planGuideSlugs(bogus, ps)
	if bg["not_a_real_guide"] || !bg["e2ee"] {
		t.Errorf("validation should drop unknown slug, keep e2ee; got %v", keys(bg))
	}
}

// TestPlanGuideSlugs_KeywordAloneCannotPullEvidenceGated: a task-text keyword
// hit ("support chat" -> manage_relay) preloads the tool schema but must NOT
// inline an evidence-gated guide core (realtime_signaling, ~20KB with its
// contracts) into every build turn. A planner-declared tool keeps pulling it.
func TestPlanGuideSlugs_KeywordAloneCannotPullEvidenceGated(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	keywordOnly := &Plan{
		Goal:  "A customer support site.",
		Tasks: []PlanTaskSpec{{Title: "Support chat panel", Intent: "A support chat widget on the contact page."}},
	}
	if got := planGuideSlugs(keywordOnly, ps); got["realtime_signaling"] {
		t.Errorf("keyword-only chat plan must not inline realtime_signaling; got %v", keys(got))
	}
	preloaded := false
	for _, n := range buildAllTools(keywordOnly) {
		if n == "manage_relay" {
			preloaded = true
		}
	}
	if !preloaded {
		t.Errorf("keyword inference should still preload the manage_relay schema")
	}

	declared := &Plan{
		Tasks: []PlanTaskSpec{{Title: "Live chat", Tools: []string{"manage_relay"}}},
	}
	if got := planGuideSlugs(declared, ps); !got["realtime_signaling"] {
		t.Errorf("planner-declared manage_relay must pull realtime_signaling; got %v", keys(got))
	}
}

// TestPlanGuideSlugs_LockedRealtimeDecisionPullsGuide: a plan that LOCKS the
// realtime-model decision must inline the realtime guide core even when the
// planner attached no model tool and named no guide - the recall gap the
// `decisions:` front-matter on the realtime guides closes. A locked decision is a
// signalLocked, stronger than the vetoed keyword signal, so it inlines. webrtc
// rides webrtc_signaling, which pulls its realtime_signaling base via `requires`.
func TestPlanGuideSlugs_LockedRealtimeDecisionPullsGuide(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	// Control: this neutral plan carries no realtime signal of any kind.
	control := &Plan{Goal: "A shared workspace.", Tasks: []PlanTaskSpec{{Title: "Board view"}}}
	if planGuideSlugs(control, ps)["realtime_signaling"] {
		t.Fatalf("control plan has no realtime signal but inlined realtime_signaling")
	}
	// Each of the four realtime_signaling-owned choices, locked alone, pulls it.
	for _, choice := range []string{"relay", "tick", "authoritative", "matchmaking"} {
		plan := &Plan{
			Goal:      "A shared workspace.",
			Tasks:     []PlanTaskSpec{{Title: "Board view"}},
			Decisions: []PlanDecision{{Topic: "realtime model", Choice: choice}},
		}
		if got := planGuideSlugs(plan, ps); !got["realtime_signaling"] {
			t.Errorf("locked realtime model=%q must inline realtime_signaling; got %v", choice, keys(got))
		}
	}
	// webrtc routes to webrtc_signaling and drags in its realtime_signaling base.
	webrtc := &Plan{
		Goal:      "A meeting place.",
		Tasks:     []PlanTaskSpec{{Title: "Room view"}},
		Decisions: []PlanDecision{{Topic: "realtime model", Choice: "webrtc"}},
	}
	got := planGuideSlugs(webrtc, ps)
	if !got["webrtc_signaling"] {
		t.Errorf("locked realtime model=webrtc must inline webrtc_signaling; got %v", keys(got))
	}
	if !got["realtime_signaling"] {
		t.Errorf("webrtc_signaling requires realtime_signaling, so it must ride along; got %v", keys(got))
	}
}

// TestPlanGuideBlockInjectsBodies: the rendered block for a 3D plan carries the
// 3d_rendering guide's body (the null-canvas renderer contract) with contracts
// transcluded, and is empty for a plan that implies nothing. The blocky-world
// recipe (voxel_worlds) is NOT pulled by three.js alone - it rides ONLY an
// explicit guides:["voxel_worlds"] opt-in, so a smooth/realistic 3D build never
// gets the blocky chunk/cube guidance.
func TestPlanGuideBlockInjectsBodies(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	plan := &Plan{
		Goal:              "A smooth 3D world",
		ExternalLibraries: []LibrarySpec{{Name: "three.js", Kind: "module", ModuleName: "three"}},
	}
	block := planGuideBlock(plan, ps)
	if !strings.Contains(block, `<guide slug="3d_rendering">`) {
		t.Fatalf("block missing the 3d_rendering guide body")
	}
	if !strings.Contains(block, "null-canvas") && !strings.Contains(block, "reading 'width'") {
		t.Error("3d_rendering guide body missing the null-canvas renderer guidance")
	}
	// three.js alone must NOT drag in the voxel/block-world recipe - otherwise a
	// smooth-terrain brief would collapse into a blocky build.
	if strings.Contains(block, `<guide slug="voxel_worlds">`) || strings.Contains(block, "fall through the floor") {
		t.Error("voxel_worlds must NOT ride a plain three.js build; it pulled the blocky-world recipe without an opt-in")
	}
	// Its smooth-terrain counterpart is the same deal: heightfield_terrain rides ONLY
	// an explicit opt-in, so a plain 3D build stays lean and neither rival terrain
	// architecture is forced on it.
	if strings.Contains(block, `<guide slug="heightfield_terrain">`) {
		t.Error("heightfield_terrain must NOT ride a plain three.js build; it needs an explicit guides opt-in")
	}
	if strings.Contains(block, "{{contract:") {
		t.Error("a {{contract:...}} token survived planGuideBlock")
	}

	// Opt-in: a plan that declares guides:["voxel_worlds"] DOES get the blocky-world
	// recipe (chunk meshing, the camera look-direction break/place snippet).
	voxelPlan := &Plan{
		Goal:              "A Minecraft-style block world",
		ExternalLibraries: []LibrarySpec{{Name: "three.js", Kind: "module", ModuleName: "three"}},
		Guides:            []string{"voxel_worlds"},
	}
	vblock := planGuideBlock(voxelPlan, ps)
	if !strings.Contains(vblock, `<guide slug="voxel_worlds">`) {
		t.Error("declared voxel_worlds guide missing from the build block")
	}
	if !strings.Contains(vblock, "applyEuler") {
		t.Error("voxel_worlds guide body missing the canonical camera look-direction snippet (the break/place sign-fix must reach BUILD when voxel is opted in)")
	}

	// Opt-in the other way: a plan that declares guides:["heightfield_terrain"] gets
	// the smooth deformable-terrain recipe (displacement mesh, sample-and-clamp
	// physics) - the path a smooth-terrain brief must land on instead of the cube
	// recipe.
	hfPlan := &Plan{
		Goal:              "A No Man's Sky-style planet you can dig into",
		ExternalLibraries: []LibrarySpec{{Name: "three.js", Kind: "module", ModuleName: "three"}},
		Guides:            []string{"heightfield_terrain"},
	}
	hfblock := planGuideBlock(hfPlan, ps)
	if !strings.Contains(hfblock, `<guide slug="heightfield_terrain">`) {
		t.Error("declared heightfield_terrain guide missing from the build block")
	}
	if !strings.Contains(hfblock, "sample-and-clamp") {
		t.Error("heightfield_terrain guide body missing the sample-and-clamp physics recipe (the smooth-terrain counterpart to voxel's isSolid must reach BUILD when opted in)")
	}

	if b := planGuideBlock(&Plan{Goal: "a static about page"}, ps); b != "" {
		t.Errorf("expected empty block for a no-capability plan; got non-empty (%d bytes)", len(b))
	}
}

// TestPlanGuideBlockDeterministic guards the prompt-cache contract: the injected
// guide block MUST be byte-identical across calls for a given plan, or the cached
// build-seed prefix flaps and every build re-pays full price. The selection uses
// maps internally (tool->guide, slug set), so this pins that the RENDER order
// comes only from Capabilities' (position, slug) order, never map iteration.
// Go randomizes map order per range, so repeated calls in one run exercise it.
func TestPlanGuideBlockDeterministic(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	// A plan that pulls many guides at once (the case most likely to flap).
	plan := &Plan{
		Goal:              "A 3D multiplayer game with accounts, private encrypted chat, calls, and paid unlocks.",
		ExternalLibraries: []LibrarySpec{{Name: "three.js", Kind: "module", ModuleName: "three"}},
		Tasks: []PlanTaskSpec{
			{Title: "Auth", Tools: []string{"manage_auth"}},
			{Title: "Realtime", Tools: []string{"manage_relay"}},
			{Title: "Checkout", Tools: []string{"manage_payments"}},
		},
		Guides: []string{"e2ee", "webrtc_signaling"},
	}
	first := planGuideBlock(plan, ps)
	if first == "" {
		t.Fatal("expected a non-empty multi-guide block")
	}
	for i := 0; i < 50; i++ {
		if got := planGuideBlock(plan, ps); got != first {
			t.Fatalf("planGuideBlock is non-deterministic (call %d differs) - breaks the prompt-cache prefix", i)
		}
	}
}

// TestGuideToolMapCoversCapabilities pins that a guide carrying a capability
// contract actually routes from a tool, so one that loses its `tools:` front-matter
// (or has it typo'd) fails CI instead of silently starving every build of that
// contract. It names the GUIDES rather than a tool->slug table: the table is
// derived from the same front-matter it would be checking, so transcribing it here
// only pins the transcription. What no derivation can notice is a
// contract-bearing guide that has become unreachable from any tool at all.
func TestGuideToolMapCoversCapabilities(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	reachable := map[string]bool{}
	for _, slugs := range guideToolMap(ps) {
		for _, slug := range slugs {
			reachable[slug] = true
		}
	}
	// Every guide whose capability is USED through a tool. A guide reached only by
	// a decision, a structural signal or the planner naming it (the terrain pair,
	// e2ee, game_feel, the frontend pair) is deliberately absent.
	for _, slug := range []string{
		"payments", "llm_endpoints", "event_actions", "auth_contract",
		"realtime_signaling", "webrtc_signaling", "file_uploads", "server_push",
		"seo_and_syndication", "media_generation", "inbound_webhooks",
		"server_functions", "connect_any_api",
	} {
		if !reachable[slug] {
			t.Errorf("guide %q is reachable from no tool - did %s/GUIDE.md lose its `tools:` front-matter?", slug, slug)
		}
	}
}

// TestClientSplitGuideTrigger: the client_js_split guide is injected only when
// the plan implies a multi-module client-JS build (a subsystem-split page or an
// engine library), and kept out of plain content/CRUD builds so the universal
// build frame stays lean.
func TestClientSplitGuideTrigger(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	// A complex page carved into a spine + subsystem leaves (the plan owns >1
	// /files JS across tasks, and a leaf EDITS the entry).
	editor := &Plan{
		Goal: "A browser video editor.",
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Editor shell", Paths: []string{"/", "/files/editor.js", "/files/editor.state.js"}},
			{ID: "timeline", Title: "Timeline", Paths: []string{"/files/editor.timeline.js"}, Edits: []string{"/files/editor.js"}, Deps: []string{"spine"}},
		},
	}
	if !planGuideSlugs(editor, ps)["client_js_split"] {
		t.Errorf("a subsystem-split page must imply %q; got %v", "client_js_split", keys(planGuideSlugs(editor, ps)))
	}

	// An app-like page a doctrine-following planner expresses over its ROUTE (a spine
	// owns /app; subsystem tasks EDIT /app; none owns a .js leaf) implies a split too
	// - even with no planner-named .js file - so BUILD gets the import-tree guide up
	// front instead of reconstructing it under save-gate pressure.
	routeApp := &Plan{
		Goal: "A team chat app.",
		Tasks: []PlanTaskSpec{
			{ID: "app", Title: "App shell", Paths: []string{"/app"}},
			{ID: "stream", Title: "Message stream", Edits: []string{"/app"}},
			{ID: "composer", Title: "Composer", Edits: []string{"/app"}},
			{ID: "threads", Title: "Threads", Edits: []string{"/app"}},
		},
	}
	if !planGuideSlugs(routeApp, ps)["client_js_split"] {
		t.Errorf("a route-funnel app page must imply %q; got %v", "client_js_split", keys(planGuideSlugs(routeApp, ps)))
	}

	// An app-shaped project (screens as client-side views of one `/` page - the
	// spine plus one view leaf per screen) implies the split guide the same way a
	// subsystem split does.
	habits := &Plan{
		Goal:      "An installable habit-tracker PWA.",
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app"}},
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "App spine", Paths: []string{"/", "/files/habits.js", "/files/habits.state.js"}},
			{ID: "today", Title: "Today view", Paths: []string{"/files/habits.today.js"}, Edits: []string{"/files/habits.js"}, Deps: []string{"spine"}},
			{ID: "streaks", Title: "Streaks view", Paths: []string{"/files/habits.streaks.js"}, Edits: []string{"/files/habits.js"}, Deps: []string{"spine"}},
			{ID: "settings", Title: "Settings view", Paths: []string{"/files/habits.settings.js"}, Edits: []string{"/files/habits.js"}, Deps: []string{"spine"}},
		},
	}
	if !planGuideSlugs(habits, ps)["client_js_split"] {
		t.Errorf("an app-shaped view-leaf plan must imply %q; got %v", "client_js_split", keys(planGuideSlugs(habits, ps)))
	}

	// An engine library implies a split too.
	game := &Plan{
		Goal:              "A 3D game.",
		ExternalLibraries: []LibrarySpec{{Name: "three.js", Kind: "module", ModuleName: "three"}},
	}
	if !planGuideSlugs(game, ps)["client_js_split"] {
		t.Errorf("an engine-library plan must imply %q; got %v", "client_js_split", keys(planGuideSlugs(game, ps)))
	}

	// A plain content/CRUD site must NOT pull the split guide.
	blog := &Plan{
		Goal: "A blog.",
		Tasks: []PlanTaskSpec{
			{ID: "posts", Title: "Posts table + API"},
			{ID: "home", Title: "Home page", Paths: []string{"/"}, Deps: []string{"posts"}},
		},
	}
	if planGuideSlugs(blog, ps)["client_js_split"] {
		t.Errorf("a plain CRUD site must NOT imply %q; got %v", "client_js_split", keys(planGuideSlugs(blog, ps)))
	}
}

// TestVisualCraftGuideTrigger: the visual_craft guide is injected for any
// UI-bearing plan (a page or canvas) and kept out of a headless API-only build,
// so every project with a screen gets the richness/variety push and a pure
// backend does not pay for it.
func TestVisualCraftGuideTrigger(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	// A plain content site (a `/` page) is UI-bearing even with no shape decision.
	blog := &Plan{
		Goal: "A blog.",
		Tasks: []PlanTaskSpec{
			{ID: "posts", Title: "Posts table + API"},
			{ID: "home", Title: "Home page", Paths: []string{"/"}, Deps: []string{"posts"}},
		},
	}
	if !planGuideSlugs(blog, ps)["visual_craft"] {
		t.Errorf("a UI plan must imply %q; got %v", "visual_craft", keys(planGuideSlugs(blog, ps)))
	}

	// An app-shaped project is UI-bearing via its shape decision.
	app := &Plan{
		Goal:      "A drawing tool.",
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "app"}},
		Tasks:     []PlanTaskSpec{{ID: "spine", Title: "Canvas", Paths: []string{"/"}}},
	}
	if !planGuideSlugs(app, ps)["visual_craft"] {
		t.Errorf("an app plan must imply %q; got %v", "visual_craft", keys(planGuideSlugs(app, ps)))
	}

	// A headless API-only project (shape locked headless, only tables/endpoints)
	// must NOT pull the visual guide.
	headless := &Plan{
		Goal:      "A webhook relay API.",
		Decisions: []PlanDecision{{Topic: "project shape", Choice: "headless"}},
		Tasks: []PlanTaskSpec{
			{ID: "events", Title: "Events table + API"},
			{ID: "relay", Title: "Webhook fan-out trigger"},
		},
	}
	if planGuideSlugs(headless, ps)["visual_craft"] {
		t.Errorf("a headless plan must NOT imply %q; got %v", "visual_craft", keys(planGuideSlugs(headless, ps)))
	}
}

// TestWindowBridgeRuleTrigger pins the compact upfront window-bridge steer: a plan
// BELOW the full client_js_split threshold but shipping two or more sibling client-JS
// files carries the rule (the multi-page-site-sharing-a-helper gap), a single-JS or
// pure-CRUD plan does not, and a plan that already pulls the full split guide is not
// double-served (the guide covers it in depth).
func TestWindowBridgeRuleTrigger(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}

	// Two sibling page modules sharing a helper, each task owning ONE .js and no
	// cross-edit - so planImpliesClientSplit is false, but the window-bridge rule fires.
	site := &Plan{
		Goal: "A marketing site with scroll-reveal animations.",
		Tasks: []PlanTaskSpec{
			{ID: "shell", Title: "Shell + nav", Paths: []string{"default", "/files/nav.js"}},
			{ID: "home", Title: "Home page", Paths: []string{"/", "/files/home.js"}, Deps: []string{"shell"}},
		},
	}
	if planGuideSlugs(site, ps)["client_js_split"] {
		t.Fatalf("precondition: a two-page site with one .js each must NOT pull the full split guide")
	}
	if !planNeedsWindowBridgeRule(site, ps) {
		t.Errorf("a plan with two sibling client-JS files must carry the window-bridge rule; got files=%d", planClientJSFiles(site))
	}

	// A single client-JS file cannot bridge across modules - no rule.
	onePage := &Plan{
		Goal: "A landing page.",
		Tasks: []PlanTaskSpec{
			{ID: "home", Title: "Home", Paths: []string{"/", "/files/home.js"}},
		},
	}
	if planNeedsWindowBridgeRule(onePage, ps) {
		t.Errorf("a single-client-JS plan must NOT carry the window-bridge rule; got files=%d", planClientJSFiles(onePage))
	}

	// A plain CRUD site with no client-JS files - no rule.
	blog := &Plan{
		Goal: "A blog.",
		Tasks: []PlanTaskSpec{
			{ID: "posts", Title: "Posts table + API"},
			{ID: "home", Title: "Home page", Paths: []string{"/"}, Deps: []string{"posts"}},
		},
	}
	if planNeedsWindowBridgeRule(blog, ps) {
		t.Errorf("a plain CRUD site must NOT carry the window-bridge rule; got files=%d", planClientJSFiles(blog))
	}

	// A subsystem split already pulls the full guide, which covers the rule in depth -
	// so the compact rule is NOT also served (no duplication).
	editor := &Plan{
		Goal: "A browser video editor.",
		Tasks: []PlanTaskSpec{
			{ID: "spine", Title: "Editor shell", Paths: []string{"/", "/files/editor.js", "/files/editor.state.js"}},
			{ID: "timeline", Title: "Timeline", Paths: []string{"/files/editor.timeline.js"}, Edits: []string{"/files/editor.js"}, Deps: []string{"spine"}},
		},
	}
	if !planGuideSlugs(editor, ps)["client_js_split"] {
		t.Fatalf("precondition: a subsystem split must pull the full split guide")
	}
	if planNeedsWindowBridgeRule(editor, ps) {
		t.Errorf("a plan already carrying the full split guide must NOT be double-served the compact rule")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestGuideCapabilityGaps pins the capability-aware prefetch: a wanted guide
// whose tools are ALL gated off is dropped with a skip note that names the
// fallback; one whose tools are partly gated survives with a note naming the
// dead ones; a guide with no tools is never dropped or noted. Fail-open on a
// nil registry.
func TestGuideCapabilityGaps(t *testing.T) {
	ps, err := pack.Embedded()
	if err != nil {
		t.Fatalf("pack.Embedded: %v", err)
	}
	reg := tools.DefaultRegistry()
	noCaps := llm.Capabilities{HasLLM: true} // no image/stock/embeddings/search

	// media_generation covers image generation, stock fetch, and transforming an
	// image one of those produced. With neither provider configured all three are
	// gated, so the guide is dropped rather than advertised: an inlined guide for
	// a capability that cannot be called is what sends the builder searching.
	// e2ee declares no tools -> untouched, no note.
	want := map[string]bool{"media_generation": true, "e2ee": true}
	keep, notes := guideCapabilityGaps(want, ps, reg, noCaps)
	if keep["media_generation"] {
		t.Errorf("media_generation must drop when no image or stock provider exists; kept %v", keep)
	}
	if !keep["e2ee"] {
		t.Errorf("a guide declaring no tools must survive; kept %v", keep)
	}
	if len(notes) != 1 {
		t.Fatalf("want exactly one note (media_generation), got %d: %v", len(notes), notes)
	}
	for _, frag := range []string{"media_generation", "manage_image_gen", "image", "manage_stockphotos", "stock", "SVG"} {
		if !strings.Contains(notes[0], frag) {
			t.Errorf("note should name %q, got: %s", frag, notes[0])
		}
	}

	// One image source is enough to bring the whole guide back, transform tool
	// included: something can now put an image in /files for it to work on.
	keep, notes = guideCapabilityGaps(want, ps, reg, llm.Capabilities{HasLLM: true, HasStock: true})
	if !keep["media_generation"] {
		t.Errorf("a stock provider alone must revive media_generation; kept %v", keep)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "manage_image_gen") || strings.Contains(notes[0], "manage_media") {
		t.Errorf("partial gap should flag only the still-dead tools, got: %v", notes)
	}

	// With every capability present, nothing is dropped or noted.
	allCaps := llm.Capabilities{HasLLM: true, HasImage: true, HasStock: true, HasSearch: true, HasEmbeddings: true, HasPush: true, HasPDF: true}
	keep, notes = guideCapabilityGaps(want, ps, reg, allCaps)
	if !keep["media_generation"] || !keep["e2ee"] || len(notes) != 0 {
		t.Errorf("full caps: want all kept and no notes, got keep=%v notes=%v", keep, notes)
	}

	// Nil registry: fail open - want returned untouched, no notes.
	keep, notes = guideCapabilityGaps(want, ps, nil, noCaps)
	if len(keep) != len(want) || len(notes) != 0 {
		t.Errorf("nil registry must fail open, got keep=%v notes=%v", keep, notes)
	}
}

// planGuideBlock pairs planGuideSlugs with renderGuidesFor over a plan's implied
// guides, in (position, slug) order - the whole selection-plus-render path in one
// call. Production renders via renderGuidesFor directly in buildIntentSeed.
func planGuideBlock(plan *Plan, ps []pack.Pack) string {
	return renderGuidesFor(planGuideSlugs(plan, ps), ps, nil)
}

// matchedGuideSlugs returns a plan's implied guide slugs, sorted.
func matchedGuideSlugs(plan *Plan, ps []pack.Pack) []string {
	return sortedSlugs(planGuideSlugs(plan, ps))
}

// A library routes its guide only when the spec names it as a whole token, so a
// declared library that merely contains the name does not drag a guide in.
func TestLibraryNamedMatchesWholeTokens(t *testing.T) {
	cases := []struct {
		spec, lib string
		want      bool
	}{
		{"three three.js https://cdn/three.module.js", "three", true},
		{"three-stdlib", "three", true},
		{"https://cdn.jsdelivr.net/npm/three@0.160.0/build/three.module.js", "three", true},
		{"phaser3 phaser 3", "phaser", true},
		{"cannon-es cannon-es", "cannon", true},
		{"threesome", "three", false},
		{"withthree", "three", false},
		{"cannonball-js", "cannon", false},
		{"phaserx", "phaser", false},
		{"", "three", false},
	}
	for _, c := range cases {
		if got := libraryNamed(c.spec, c.lib); got != c.want {
			t.Errorf("libraryNamed(%q, %q) = %v, want %v", c.spec, c.lib, got, c.want)
		}
	}

	ps, _ := pack.Embedded()
	imposter := &Plan{ExternalLibraries: []LibrarySpec{{Name: "threesome-ui", Kind: "module", ModuleName: "threesome"}}}
	if _, ok := planGuideSignals(imposter, ps)["3d_rendering"]; ok {
		t.Error("a library whose name only contains \"three\" must not route the 3d_rendering guide")
	}
	real := &Plan{ExternalLibraries: []LibrarySpec{{Name: "three.js", Kind: "module", ModuleName: "three"}}}
	if _, ok := planGuideSignals(real, ps)["3d_rendering"]; !ok {
		t.Error("three.js must still route the 3d_rendering guide")
	}
}
