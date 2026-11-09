package pack

import (
	"strings"
	"testing"

	"openfabrica/contracts"
)

func embedded(t *testing.T) []Pack {
	t.Helper()
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	return ps
}

func TestEmbeddedLoads(t *testing.T) {
	ps := embedded(t)
	// 4 stage frames + the lean capability-guide core + the _common_rules
	// fragment. Golden paths were folded away when the guide layer collapsed.
	if len(ps) < 10 {
		t.Fatalf("loaded %d packs, want >= 10", len(ps))
	}
	var stage, cap, frag int
	for _, p := range ps {
		switch p.Role {
		case RoleStage:
			stage++
		case RoleCapability:
			cap++
		case RoleFragment:
			frag++
		default:
			t.Errorf("pack %q has unknown role %q", p.Slug, p.Role)
		}
		if p.Body == "" {
			t.Errorf("pack %q has empty body", p.Slug)
		}
	}
	if stage != 4 {
		t.Errorf("got %d stage packs, want 4", stage)
	}
	if frag < 1 {
		t.Errorf("got %d fragment packs, want >= 1", frag)
	}
	// Lean guide core: capability guides are the load-bearing platform CONTRACTS
	// (wire shapes and architecture facts the build must match - auth, REST
	// envelope, realtime, e2ee, webrtc, trigger event catalogue, payment, and the
	// non-CRUD endpoint families whose route+consumption shape no tool schema
	// conveys: uploads, server-push/SSE, inbound webhooks, SEO/syndication, and
	// custom server functions - server_functions, whose sandboxed handler contract
	// (synchronous-only handler(req,ctx), the ctx.query/insert/fetch/secret API, and
	// declared-writes-applied-in-one-tx-after-return) is a real wire shape a strong
	// model gets wrong from the 130-char OneLiner alone, invisible to every gate) PLUS a
	// few CROSS-TOOL FLOWS no single tool's WhenToUse can carry end to end (hosted
	// AI + RAG, build-time media, connect_any_api - the secrets -> provider ->
	// request flow plus the build-time vs runtime split, where the running app must
	// go through a server function's ctx.secret/ctx.fetch: a fact spanning three
	// tools that no single WhenToUse carries). Generic how-to and single-tool
	// guidance live in the tool defs and stage frame, NOT here. Guard against
	// silent re-bloat: before raising this, confirm the new guide carries a fact
	// the model genuinely cannot get from a tool's WhenToUse or the build frame.
	// Opt-in recipe guides (voxel_worlds, heightfield_terrain, game_feel) earn a
	// slot only when their absence steers a build into the wrong architecture -
	// game_feel earns its slot because without it every generated game collapses
	// to the same default look; a guide like client_js_split earns one when the fact
	// lived only in save-gate rejection strings, not any WhenToUse or frame.
	// agent_endpoints earns a slot: the multi-step run contract (the /run loop,
	// the tool-allowlist model, stop_reason, and the whole-run budget) is an
	// architecture the model cannot reconstruct from manage_agent's OneLiner alone.
	// client_view_toggle earns a slot (same footing as client_js_split): the
	// one-visibility-convention rule was stated nowhere the model reads except the new
	// dead_toggle gate's rejection string, not in any tool WhenToUse or frame - and
	// without it generated apps keep shipping panels/nav whose handler fires but
	// whose reveal class the CSS never shows, so the control looks dead.
	// phaser_2d earns a slot: without a second engine guide every 2D brief
	// builds on three.js or bare canvas (the only paved road), and the
	// no-image-assets texture pipeline plus the 3.60 particle API break exist
	// nowhere else. physics_3d earns a slot the same way: the body/mesh pairing
	// and fixed-step rules live in no tool WhenToUse, and without them physics
	// briefs hand-roll unstable integrators or bolt physics onto terrain walking.
	// user_flows earns a slot as an OFFLOAD, not new prose: the of_test.js step
	// reference (~4KB) rode every turn inside the preloaded manage_testing schema
	// while it is needed once per build; as a guide's extended section it is recalled
	// on demand, and the always-on core is a few lines.
	if cap > 26 {
		t.Errorf("got %d capability guides; the lean guide core should stay small (<=26 load-bearing contracts/flows)", cap)
	}
	t.Logf("packs: %d stage, %d capability, %d fragment", stage, cap, frag)
}

// TestOneStagePackPerStage: exactly one role:stage pack per build stage.
func TestOneStagePackPerStage(t *testing.T) {
	ps := embedded(t)
	seen := map[string]int{}
	for _, p := range ps {
		if p.Role == RoleStage {
			seen[p.Stage]++
		}
	}
	for _, st := range []string{"build", "plan", "live", "expand"} {
		if seen[st] != 1 {
			t.Errorf("stage %q has %d stage packs, want 1", st, seen[st])
		}
	}
}

// TestContractRefsResolve: every {{contract:id}} token in any pack body resolves
// to a real contract. Transclusion is token-driven, so the token is the only
// declaration there is.
func TestContractRefsResolve(t *testing.T) {
	all, err := contracts.Load()
	if err != nil {
		t.Fatalf("contracts.Load: %v", err)
	}
	ps := embedded(t)
	for _, p := range ps {
		for _, id := range contractTokens(p.Body) {
			if _, ok := all[id]; !ok {
				t.Errorf("pack %q body references unknown {{contract:%s}}", p.Slug, id)
			}
		}
	}
}

// TestEmbeddedAssembleDeterministic: Select+Assemble over the real tree is
// byte-deterministic (the prompt-cache contract) and the always-on platform
// layer is identical across stages (the shared-cache-prefix contract).
func TestEmbeddedAssembleDeterministic(t *testing.T) {
	ps := embedded(t)
	build := func() string {
		return Assemble(AssembleInput{Stage: "build", Wave: "w", Selection: Select(ps, "build")}).System
	}
	s1, s2 := build(), build()
	if s1 != s2 {
		t.Error("Assemble over the embedded tree is not deterministic")
	}
	// The laws are transcluded: no raw {{contract:...}} or {{GUIDE_INDEX}} token
	// may survive into the assembled system prompt.
	if strings.Contains(s1, "{{contract:") || strings.Contains(s1, guideIndexToken) {
		t.Error("an unresolved {{contract:...}}/{{GUIDE_INDEX}} token survived assembly")
	}
	// A law's body (the REST envelope) is present, transcluded from the contract.
	if !strings.Contains(s1, "## Platform laws") {
		t.Error("mental model laws section missing from the system prompt")
	}
	// BUILD's guides arrive prefetched in its seed, so its index is a pointer at
	// the shelf rather than the per-guide list every turn would re-read.
	if !strings.Contains(s1, "manage_guides action=list") {
		t.Error("BUILD's guide index pointer is missing")
	}
	if strings.Contains(s1, "`e2ee`") {
		t.Error("BUILD should not carry the per-guide index; its guides are inlined in the seed")
	}
	if strings.Contains(s1, `<guide slug="e2ee">`) {
		t.Error("guides must not be inlined as full bodies any more")
	}

	// The always-on platform layer (everything before the stage's role line) is
	// identical across stages - the shared cached prefix.
	planSys := Assemble(AssembleInput{Stage: "plan", Wave: "w", Selection: Select(ps, "plan")}).System
	if prefixA, prefixB := alwaysOn(s1), alwaysOn(planSys); prefixA != prefixB {
		t.Error("the always-on platform layer differs between build and plan; the shared cache prefix is broken")
	}
	// PLAN picks its own guides, so it keeps the full per-guide list.
	if !strings.Contains(planSys, "`e2ee`") {
		t.Error("expected e2ee listed in PLAN's guide index")
	}
}

// alwaysOn returns the always-on platform layer: everything up to the first
// stage role line (the first "# How this platform works" through the guide
// index, before the per-stage frame). Heuristic: cut at the guide index's end is
// hard, so we compare the slice up to the "## Guide index" marker plus the index
// body, which are both stage-independent.
func alwaysOn(system string) string {
	const marker = "## Guide index"
	i := strings.Index(system, marker)
	if i < 0 {
		return system
	}
	return system[:i]
}

// TestPlanInlinesGuideCores: RenderGuidesCore (the PLAN/BUILD block) carries each
// guide's CORE - including the realtime `## Decision:` block and transcluded
// contracts - so the planner locks architecture with the decision criteria in
// front of it, plus a recall index of the EXTENDED sections it holds back. The
// extended bodies must NOT be inlined (that is the ~40% core-only trims). BUILD's
// assembled system prompt still inlines no guide bodies at all.
func TestPlanInlinesGuideCores(t *testing.T) {
	ps := embedded(t)
	core := RenderGuidesCore(Capabilities(ps))
	if !strings.Contains(core, "Decision: realtime model") {
		t.Error("RenderGuidesCore is missing the realtime model decision block the planner needs (it must live in the core)")
	}
	if strings.Contains(core, "{{contract:") {
		t.Error("a {{contract:...}} token survived RenderGuidesCore")
	}
	if !strings.Contains(core, "action=recall") {
		t.Error("RenderGuidesCore omitted the recall index for guides that hold extended sections")
	}
	// Prove the extended bodies are held back: a guide with sections must have its
	// section body absent from the core render, but its anchor present (in the index).
	rt := BySlug(ps, "realtime_signaling")
	if rt == nil || len(rt.Sections) == 0 {
		t.Fatal("expected realtime_signaling to carry extended sections")
	}
	last := rt.Sections[len(rt.Sections)-1]
	if strings.Contains(core, strings.TrimSpace(last.Body)) {
		t.Errorf("RenderGuidesCore inlined extended section %q body - core-only inlining must hold it back", last.Anchor)
	}
	if !strings.Contains(core, last.Anchor) {
		t.Errorf("RenderGuidesCore recall index is missing extended anchor %q", last.Anchor)
	}
	// BUILD gets the index only - no guide bodies.
	buildSys := Assemble(AssembleInput{Stage: "build", Wave: "w", Selection: Select(ps, "build")}).System
	if strings.Contains(buildSys, "<guide slug=") {
		t.Error("the build system prompt inlined guide bodies; build should use the index + recall")
	}
}

// TestRealtimeDecisionFacetCarriesTickConsequence pins that the realtime decision
// block states the CONSEQUENCE of a non-authoritative model, not just how to pick
// one: `tick`/`relay` run no server code, so roster ownership / assignment /
// validation is authoritative or client-side, never a reducer on a tick task. The
// planner reads only the decision facet (RenderGuidesDecision -> choiceBlocks), so
// this guidance is load-bearing ONLY while it lives inside the `## Decision:` block;
// if it drifts into an extended section or ordinary core prose, choiceBlocks drops
// it and the planner is back to conflating the two architectures (the "authoritative
// tick + server reducer" plan). Asserting on the rendered facet fails the moment
// that happens.
func TestRealtimeDecisionFacetCarriesTickConsequence(t *testing.T) {
	ps := embedded(t)
	rt := BySlug(ps, "realtime_signaling")
	if rt == nil {
		t.Fatal("realtime_signaling guide not found")
	}
	dec := RenderGuidesDecision([]Pack{*rt})
	for _, want := range []string{
		"there is no server code there to run it",
	} {
		if !strings.Contains(dec, want) {
			t.Errorf("realtime decision facet is missing the tick/relay consequence %q - it must stay inside the "+
				"`## Decision:` block so the planner reads it at plan time; got:\n%s", want, dec)
		}
	}
}

// contractTokens extracts the ids referenced by {{contract:<id>}} tokens.
func contractTokens(body string) []string {
	const open = "{{contract:"
	var out []string
	for {
		i := strings.Index(body, open)
		if i < 0 {
			break
		}
		rest := body[i+len(open):]
		j := strings.Index(rest, "}}")
		if j < 0 {
			break
		}
		out = append(out, strings.TrimSpace(rest[:j]))
		body = rest[j+2:]
	}
	return out
}
