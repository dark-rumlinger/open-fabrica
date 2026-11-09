package pack

import (
	"strings"
	"testing"
)

// guideCoreTotalBudget caps the rendered bytes of every capability core together,
// contracts transcluded and wrappers included - the payload BUILD inlines when a
// project happens to select everything, and the one number that prices a guide
// growing back. The stage-frame budgets cannot see it: TestStageFrameSizeBudget
// empties {{GUIDES_CATALOGUE}} so it measures hand-authored frame prose only.
// Set to ~1.03x the figure the last trim landed on. Over budget means find the
// rule's one home and cut the copy; raising the cap is the last resort.
// Bumped when the agent_endpoints guide was added, and again for
// client_view_toggle (both genuinely new capabilities, not restatements).
// Lowered after the reducer contract moved to an extended section.
// Bumped for the game_feel depth pass (juice vocabulary, game states, art
// direction menu): genuinely new guidance, not a restatement.
// Bumped again for the phaser_2d and physics_3d capability guides.
// Lowered after realtime_signaling moved persistence, room switching and identity
// below <!-- extended --> (they are situational, and recall serves them on demand).
// Bumped when client_js_split named the window_dead_global reject alongside
// window_global_bridge (the dead-read variant), so the guide points at both codes.
// Bumped when event_actions spelled out ai_embed (needs a vector index first, config {}
// on data.insert/update): the one-line version left the model guessing its config.
// Bumped when realtime_signaling warned that ofnet's clientId() export is the private
// reconnect secret, not the player's id (net.myId) - the import line advertised it.
// Bumped for the user_flows guide's small core: it replaced ~4KB of the of_test.js step
// reference that rode every BUILD turn inside the preloaded manage_testing schema (a net
// per-turn saving; the reference itself is an extended, recall-on-demand section).
// Lowered after client_view_toggle moved its overlay hit-testing and entrance-animation
// traps to extended sections, and client_js_split dropped its restated specifier rule
// and Owns-path bullet (both enforced elsewhere).
// Lowered again when api_response_contract's core kept only paging, search and the
// scope rule: its filter/addressing/ownership detail restated manage_api's routing
// prose (which rides every data build's system prompt) and is now recall-on-demand.
// Bumped by 200 for the recall-index lines of three new guide sections (column
// exposure in api_response_contract, what /register accepts and password reset in
// auth_contract). The index is the only way the model learns a section exists, so
// the bytes carry guidance for features that did not exist before (writable_columns,
// signup_columns, the platform password-reset routes), not a restatement.
// Bumped by 300 for the user_flows core: the size limit on of_test.js and the rule that a
// flow never justifies changing security, sign-in or intended behavior. Both govern every
// UI build and have no other home that is always in context.
// Bumped by 100 for the "unless the owner turned user-flow tests off" qualifier on the user_flows core.
// Bumped by 200 for the manage_chat form example in the payments core: the tool now takes
// question_type "form", and the model needs the shape of fields to ask for two keys in one card.
// Bumped by 4500 for the guide <-> implementation drift audit: every added line corrects a
// claim the code contradicted (audio not in the upload default, provider_type default,
// register not draining the fragment, matched/state not reaching net.on, and similar), so
// the bytes replace wrong guidance rather than restating a rule. The situational detail
// went to extended sections.
// Bumped by 200 (to 142_200) after re-syncing the encrypted knowledge mirror
// (go generate ./knowledge): the embedded cores had drifted behind edited plaintext
// GUIDE.md files, so the regenerate surfaced 176 bytes of already-authored guidance
// the stale mirror had been hiding, not a new restatement.
const guideCoreTotalBudget = 142_200

// TestGuideCoreTotalBudget is the ratchet on that number. Like the frame budgets it
// never reads the words, so a restatement is priced whatever it is called.
func TestGuideCoreTotalBudget(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	got := len(RenderGuidesCore(Capabilities(ps)))
	t.Logf("rendered capability cores: %d bytes (budget %d)", got, guideCoreTotalBudget)
	if got > guideCoreTotalBudget {
		t.Errorf("rendered guide cores are %d bytes, over the %d budget by %d - collapse a restatement "+
			"to its single home (knowledge/stages/AUTHORING.md) rather than raising the cap",
			got, guideCoreTotalBudget, got-guideCoreTotalBudget)
	}
	if slack := float64(guideCoreTotalBudget-got) / float64(got); slack > 0.15 {
		t.Logf("budget has drifted %.0f%% loose - lower guideCoreTotalBudget to %d", slack*100, int(float64(got)*1.03))
	}
}

// TestNoDecisionInExtended locks the invariant RenderGuidesCore depends on: a
// guide's `## Decision:` block (the architecture choice PLAN must lock) MUST live
// in the CORE, never in a recall-on-demand extended section. If a decision block
// drifted below `<!-- extended -->`, core-only inlining would stop showing it to
// the planner and a locked architecture choice would silently vanish from the
// PLAN prompt. Author decisions above the divider.
func TestNoDecisionInExtended(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	for _, g := range Capabilities(ps) {
		for _, s := range g.Sections {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(s.Title)), "decision") ||
				strings.Contains(strings.ToLower(s.Body), "\n## decision") {
				t.Errorf("guide %q has a Decision block in extended section %q - move it above <!-- extended --> "+
					"so core-only inlining keeps it in the PLAN prompt", g.Slug, s.Anchor)
			}
		}
	}
}

// TestEveryDecisionBlockIsPlanRecordable holds the other half of the invariant.
// stages/plan/STAGE.md tells the planner, twice and in MUST terms, to record a
// `decisions` entry for every `## Decision:` block an inlined guide carries. That
// only works while every such block IS a plan-time architecture choice: a heading
// used for a BUILD-time implementation choice (stream vs whole answer, one-shot vs
// registered provider) hands the planner a MUST it can only satisfy with noise,
// and teaches it that some Decision blocks are optional - which is the reading that
// loses a real one. Those are `## Choose:` blocks; a `## Decision:` says so in its
// heading.
func TestEveryDecisionBlockIsPlanRecordable(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	const marker = "record in the plan's `decisions`"
	for _, g := range Capabilities(ps) {
		for _, line := range strings.Split(g.Body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "## Decision:") {
				continue
			}
			if !strings.Contains(line, marker) {
				t.Errorf("guide %q heading %q must say %q - or, if it is a BUILD-time implementation "+
					"choice rather than an architecture decision PLAN locks, be a `## Choose:` heading instead. "+
					"stages/plan/STAGE.md makes recording every `## Decision:` block mandatory.", g.Slug, line, marker)
			}
		}
	}
}

// TestNoEmptyGuideCores fails the build if any capability guide's CORE trims to
// empty after contract substitution. RenderGuidesCore skips such a guide (an empty
// `<guide>` block is malformed, zero-guidance markup), so a plan that implies the
// guide would build with NO inlined guidance and no error - exactly the silent
// regression the runtime skip guards against. This test is the first line of
// defense: keep every guide's core substantive, above `<!-- extended -->`.
func TestNoEmptyGuideCores(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	for _, g := range Capabilities(ps) {
		if strings.TrimSpace(substituteContracts(g.CoreBody())) == "" {
			t.Errorf("guide %q has an empty CORE after contract substitution - RenderGuidesCore would emit nothing "+
				"for it, so a plan implying this guide builds with no inlined guidance. Add core content above <!-- extended -->.", g.Slug)
		}
	}
}

// perGuideRenderedCap caps ONE guide's rendered core, contracts transcluded.
// maxGuideCoreLines cannot see transclusion (a short core can render tens of
// KB of contract), and guideCoreTotalBudget prices only the corpus sum, where
// one guide's growth hides behind another's trim. A backstop, not a style
// budget: over it, move depth below <!-- extended --> or split the contract.
const perGuideRenderedCap = 18_000

// TestPerGuideRenderedSize is the per-guide ratchet on rendered bytes, the
// transclusion-aware complement to TestGuideCoreNotOversized.
func TestPerGuideRenderedSize(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	for _, g := range Capabilities(ps) {
		got := len(RenderGuidesCore([]Pack{g}))
		t.Logf("guide=%-22s rendered core %6d bytes", g.Slug, got)
		if got > perGuideRenderedCap {
			t.Errorf("guide %q renders %d bytes of core (cap %d) - a transcluded contract or new section "+
				"ballooned it; move situational depth below <!-- extended --> rather than raising the cap",
				g.Slug, got, perGuideRenderedCap)
		}
	}
}

// maxGuideCoreLines is a generous backstop against a guide CORE growing into an
// unscannable wall. It is NOT a style budget: it fires only when a core balloons
// far past the working range - the signal to move situational depth below
// `<!-- extended -->` per knowledge/stages/AUTHORING.md, NOT to raise this cap.
const maxGuideCoreLines = 150

// TestGuideCoreNotOversized fails when a capability guide's CORE - the slice that
// rides the PLAN/BUILD prompt in full whenever the guide is implied - exceeds the
// backstop. A gross regression (an extended section pulled back into core, or a
// new mega-guide) fails CI, so capability cores stay lean.
func TestGuideCoreNotOversized(t *testing.T) {
	ps, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	for _, g := range Capabilities(ps) {
		if n := lineCount(g.CoreBody()); n > maxGuideCoreLines {
			t.Errorf("guide %q core is %d non-empty lines (backstop %d) - move situational sections below "+
				"<!-- extended --> so core-only inlining stays lean (knowledge/stages/AUTHORING.md); do not just raise the cap.",
				g.Slug, n, maxGuideCoreLines)
		}
	}
}

// lineCount returns the number of non-empty lines in s (blank lines don't add
// prompt weight worth tracking).
func lineCount(s string) int {
	n := 0
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}
