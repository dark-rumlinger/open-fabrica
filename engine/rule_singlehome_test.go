package engine

// Rule single-home guard: a registry-driven check that each rule is authored in
// exactly one place, anchored on identifiers rather than curated prose lists.
//
// knowledge/stages/AUTHORING.md names the failure mode: the same rule authored
// three or four times in slightly different words, the copies drifting. That
// redundancy, not the rules themselves, is what confused the model.
//
// A guard that only caught VERBATIM copies (TestNoCrossPackDuplication) or a
// hand-listed phrase would leave an author free to PARAPHRASE to stay green, and
// paraphrase is the WORSE failure: a verbatim repeat reads as emphasis, while two
// wordings read as two different rules.
//
// This closes that by anchoring on IDENTIFIERS. A rule worth guarding is about
// platform nouns (project_state, `get`, fresh=true, a tool name, a version pin),
// and an author paraphrasing cannot drop the noun the rule is ABOUT. Requiring two
// token CLASSES to co-occur in one authored block survives rewording while staying
// precise: `authoritative` alone hits ~20 blocks in realtime_signaling, and the
// reread conjunct filters every one. Same insight as gate_code_singlehome_test.go,
// where the reject CODE token is the anchor the prose lacks.
//
// LIMITATIONS, do not oversell this:
//   - It CANNOT decide semantic equivalence. A paraphrase that drops both anchors
//     ("the file list you were handed is accurate") passes. detectPhrase entries
//     are no better than a hand-listed phrase, and labelled so.
//   - It CANNOT see CONTRADICTION (an absolute phrasing of a rule vs the nuanced
//     one). That is what made guards.go's re-orient nudge read as a conflict; only
//     a human resolves that.
//   - It is a RATCHET, not a cleanup: it stops the next restatement, and `known`
//     makes existing debt legible. Green != coherent.
//
// Deliberately paired with TestStageFrameSizeBudget (pack/promptsize_test.go): a
// lexical guard still rewards an author reaching for a thesaurus, while a byte
// budget prices a restatement regardless of wording, because it never reads words.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"openfabrica/contracts"
	"openfabrica/knowledge"
	"openfabrica/tools"
)

// detectKind records how much detection a rule actually gets, so the coverage
// sub-test can report it rather than let the registry's shape imply rigor.
type detectKind int

const (
	// detectAnchored: the rule's vocabulary carries platform identifiers a paraphrase
	// cannot drop. The only kind that catches a reworded restatement.
	detectAnchored detectKind = iota
	// detectPhrase: pure prose, no identifier anchor. A reworded copy shares no token,
	// so this degrades to the verbatim canary it replaces. A copy-paste ratchet only.
	detectPhrase
)

// class is one bag of surface forms for a single CONCEPT. Any variant hitting marks
// the class present in that block.
type class struct {
	name     string
	variants []string
}

// signature detects a STATEMENT of a rule inside ONE authored block: every class in
// all must hit the same block, and no veto token may be present.
type signature struct {
	all  []class
	veto []string
	kind detectKind
}

// site is one place a rule may legitimately appear. Keyed on file + count, never on
// line numbers: any edit above a block shifts its line, and a line-keyed baseline
// would be pure churn. Failures still print exact file:line.
type site struct {
	file string // display path, as blocks report it
	max  int    // blocks in this file that may state the rule; 0 = unbounded here
	note string
}

// rule is one entry: id -> home -> signature. Adding a rule is ONE entry, replacing
// the two divergent hand-maintained lists.
type rule struct {
	id   string
	home site
	sig  signature
	// pointers: AUTHORING.md allows "at most one pointer line". Declared so the
	// allowance is countable rather than aspirational. A genuine pointer names the
	// rule without stating its mechanism, so it cannot hit a full conjunction anyway.
	pointers []site
	// known: MEASURED debt - sites that state the rule and are not yet collapsed.
	// Shrinking this list IS the cleanup. A known entry that STOPS matching also
	// fails, so the ratchet turns one way only.
	known []site
	why   string
}

const (
	mentalModel = "knowledge/mental-model.md"
	commonRules = "knowledge/stages/_common_rules/STAGE.md"
	buildFrame  = "knowledge/stages/build/STAGE.md"
	planFrame   = "knowledge/stages/plan/STAGE.md"
	splitGuide  = "knowledge/guides/frontend/client_js_split/GUIDE.md"
	renderGuide = "knowledge/guides/rendering/3d_rendering/GUIDE.md"
	// realtimeGuide is the base every other realtime guide declares in `requires:`,
	// so it rides the prompt whenever webrtc_signaling or e2ee do.
	realtimeGuide = "knowledge/guides/realtime/realtime_signaling/GUIDE.md"
	// snapshotRender is project_state's own prose. It rides every build turn, so a
	// rule about a section of the snapshot is cheapest stated there, next to the data.
	snapshotRender = "snapshotbuild/render.go"
)

var ruleRegistry = []rule{
	{
		id:   "build_loop/state_is_ground_truth",
		home: site{file: commonRules, max: 1, note: "AUTHORING.md's 'Build-loop behavior' row designates this fragment as the home."},
		why: "project_state and the write echo are ground truth; re-read only when you no longer hold " +
			"the file's current bytes. The nuance must exist exactly ONCE: absolute copies " +
			"(\"never get -> patch -> get\") contradict the RepeatGuard re-orient nudge, which " +
			"correctly tells a wedged model to `get fresh=true`.",
		sig: signature{
			kind: detectAnchored, // anchored on project_state / `get` - tool and arg identifiers
			all: []class{
				{name: "trust-signal", variants: []string{
					"ground truth", "current truth", "is truth", "authoritative",
					"trust this", "trust your", "trust those", "trust the",
				}},
				// Re-READING a file only. "re-probe" is deliberately NOT here: re-probing
				// an endpoint with manage_testing is a different action, and the build
				// frame's adjacent create-tool rule ("create tools return their live
				// contracts; trust those returns, never re-probe") is a different rule
				// with its own home.
				{name: "reread-verb", variants: []string{
					"re-read", "reread", "re-`get`", "re-get", "refetch", "re-fetch",
					"get -> patch -> get", "read it back", "read them back",
				}},
			},
			// The realtime domain owns "authoritative" (manage_authoritative, the
			// server-reducer model). The reread conjunct already filters those blocks;
			// this is belt-and-braces so a future reread word in that guide cannot
			// silently arm the rule.
			veto: []string{"manage_authoritative", "authoritative endpoint", "server-reducer"},
		},
	},
	{
		id:   "build_loop/narrate_one_sentence",
		home: site{file: commonRules, max: 1, note: "_common_rules is stages:[build,live], so both stages already carry it."},
		why: "One short warm sentence before each action, never numbered steps or a tool-by-tool plan. " +
			"The live frame re-authored the whole rule as its own 'Narrate as you go' block; what is " +
			"genuinely live-only (skip the greeting, close with a one-line summary) is not this rule.",
		sig: signature{
			kind: detectAnchored, // narration trigger + the per-action cadence claim
			all: []class{
				{name: "narrate-trigger", variants: []string{
					"announce each", "before each action", "before every tool call", "before each tool call",
					"narrate", "what you're about to do", "what you are about to do",
				}},
				{name: "cadence", variants: []string{
					"one short", "one sentence", "one line per action", "one warm sentence",
					"single sentence", "one natural line",
				}},
			},
			// A block that defers to the ground rules is a pointer, not a copy. Naming
			// the rule's home is exactly what AUTHORING.md asks a pointer to do.
			veto: []string{"ground rules"},
		},
	},
	{
		id:   "build_loop/no_tool_names_to_owner",
		home: site{file: commonRules, max: 1},
		why:  "Owner-visible text describes work in plain terms; it never names a tool.",
		sig: signature{
			kind: detectAnchored,
			all: []class{
				{name: "prohibition", variants: []string{"never name", "don't name", "do not name", "never mention"}},
				{name: "object", variants: []string{"a tool", "tool in owner", "tool name", "tools to the owner"}},
			},
		},
	},
	{
		id:   "guides/no_raw_websocket",
		home: site{file: realtimeGuide, max: 1, note: "realtime_signaling owns the connect() contract, and both webrtc_signaling and e2ee declare `requires: [realtime_signaling]`, so it is already in the prompt whenever they are."},
		why: "connect() is the only authenticating client; a raw socket cannot send a bearer. " +
			"The rule was spelled out in three realtime guides that always ship together, each " +
			"deriving the ?token= mechanism again in different words. The raw_ws_auth validator " +
			"is the backstop.",
		sig: signature{
			kind: detectAnchored, // the raw-socket constructor plus the auth mechanism it cannot do
			all: []class{
				{name: "raw-socket", variants: []string{"new websocket("}},
				{name: "auth-mechanism", variants: []string{
					"?token=", "authorization header", "requires_auth", "cannot authenticate",
				}},
			},
		},
	},
	{
		id:   "build_loop/preloaded_tools",
		home: site{file: commonRules, max: 1, note: "AUTHORING.md's 'Build-loop behavior' row designates this fragment as the home, and it rides both the build and live system prompts."},
		why: "The core set arrives preloaded and must not be find_tool'd. The seed builders " +
			"restated it on the always-loaded header, the per-plan tool header and the deferred " +
			"block, so the same sentence shipped three more times in the prompt that already " +
			"carries the fragment stating it.",
		sig: signature{
			kind: detectAnchored, // the find_tool identifier plus a claim about load state
			all: []class{
				{name: "preloaded-state", variants: []string{
					"already loaded", "already be loaded", "pre-loaded", "preloaded", "already in your tool list",
				}},
				{name: "findtool-prohibition", variants: []string{
					"never `find_tool`", "never find_tool", "don't `find_tool`", "do not `find_tool`",
					"no need to `find_tool`", "never re-request",
				}},
			},
		},
	},
	// A version pin is the purest anchored rule: the token IS the fact, so paraphrase
	// is impossible. Bump it in the guide AND here, nowhere else.
	{
		id:   "guides/threejs_version_pin",
		home: site{file: renderGuide, max: 0},
		why:  "The pinned Three.js release is stated once; every other layer references the guide.",
		sig:  signature{kind: detectAnchored, all: []class{{name: "pin", variants: []string{"three@0.185.1"}}}},
	},
	// ---- single-variant detectPhrase entries ------------------------------------
	// Each anchors on one authored phrase and is counted per BLOCK, not per pack.
	// Upgrade one to detectAnchored when it earns an identifier anchor.
	{
		id:       "build_loop/patch_vs_save_decision",
		home:     site{file: commonRules, max: 1},
		pointers: []site{{file: buildFrame, max: 1, note: "the build frame points at the rule and defers the body."}},
		why: "ONE decision rule picks the edit shape - patch what you hold (fresh get or untruncated " +
			"write echo), save what you don't or for a structural rewrite. Stated once so the " +
			"patch-first flip cannot drift back into per-frame re-save steers.",
		// Anchored on the two edit VERBS plus the freshness test that picks between
		// them. A restatement has to name both actions to state the rule at all,
		// while a pointer names only the rule ("the ground rules' one decision rule")
		// and hits at most one class.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "edit-verbs", variants: []string{"`patch`", "patch with an anchor", "action=\"patch\"", "-> `patch`"}},
			{name: "freshness-test", variants: []string{
				"exact current bytes", "bytes you cannot currently see", "current bytes",
				"untruncated write echo", "fresh get",
			}},
		}},
	},
	{
		id: "build/reuse_design_system",
		// The debt this entry recorded is now collapsed: the build frame's copy is gone and
		// the home moved to where the rule was already stated TWICE anyway. The snapshot's
		// Design system / Page-scope styles explainers sit next to the vocabulary they
		// govern and ride every build turn, so they are the cheapest place to read this -
		// a frame copy paid for the same words again somewhere the model reads less often.
		home: site{file: snapshotRender, max: 2, note: "the Design system and Page-scope styles explainers each state reuse-don't-invent beside the vocabulary they list."},
		why:  "The design system is reused, not reinvented per page.",
		// Anchored: the rule is ABOUT the shared stylesheet, so a paraphrase cannot drop
		// the noun, and it only becomes a RULE when paired with the anti-pattern it bans.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "shared-css", variants: []string{"design system", "global stylesheet", "global css"}},
			{name: "ad-hoc-ban", variants: []string{
				"ad-hoc", "re-invent", "reinvent", "inventing", "invent a", "do not invent", "don't invent",
			}},
		}},
	},
	{
		id:   "build_loop/one_turn_per_item",
		home: site{file: buildFrame, max: 1, note: "the Act step carries the batching rule AND the round-trip cost that motivates it."},
		why: "One item's independent calls go out in ONE turn, because every round-trip re-sends the " +
			"whole conversation. The ground rules and freshBuildInstruction each carried their own " +
			"wording of it, so the same rule shipped three times in one prompt.",
		// Anchored on the batching instruction plus the per-round-trip cost that is the
		// whole reason for it: a pointer names the rule and hits at most one class.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "batch", variants: []string{
				"batch every independent", "batch every", "in one response", "in one turn",
				"fewest turns", "one turn -",
			}},
			{name: "turn-cost", variants: []string{
				"round-trip", "re-sends the whole conversation", "lever on build cost",
			}},
		}},
	},
	{
		id:   "build/forms_need_a_sink",
		home: site{file: buildFrame, max: 1},
		why:  "A page that captures input needs somewhere for it to land.",
		sig:  signature{kind: detectPhrase, all: []class{{name: "phrase", variants: []string{"never let a form post into the void"}}}},
	},
	{
		id:   "build/no_llm_heartbeat",
		home: site{file: buildFrame, max: 1},
		why:  "A prompt schedule wakes the full agent on every fire; upkeep uses a native_action.",
		// Anchored on the two schedule KINDS: the rule is the choice between them, so a
		// restatement has to name the one it forbids and the one it prescribes.
		// "heartbeat" alone joins the anchor set: the rule IS the ban, and a restatement
		// that drops the words "prompt schedule" still has to name the thing it forbids.
		// Paired with native_action it cannot fire on a mention that is not the rule.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "llm-schedule", variants: []string{"`prompt` schedule", "prompt schedule", "llm heartbeat", "heartbeat"}},
			{name: "native-alternative", variants: []string{"native_action"}},
		}},
	},
	{
		id:   "testing/flow_coverage_by_project_kind",
		home: site{file: "knowledge/guides/frontend/user_flows/GUIDE.md", max: 2, note: "the user_flows guide's writing-flows section carries what to cover per kind of project; its step-vocabulary list names the same two verbs, so the home counts twice."},
		why: "What of_test.js covers for each kind of project (a game expectAnimating then expectChanged, " +
			"a chat app acting as a second user, ...) was stated in the build frame AND the write_flows doc " +
			"in two wordings; it now lives in the user_flows guide (recalled on demand, off the per-turn schema).",
		// Anchored on two flow-DSL verbs a coverage list cannot avoid naming: a pointer
		// ("its doc lists what to cover") names neither.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "animating", variants: []string{"expectanimating"}},
			{name: "changed", variants: []string{"expectchanged"}},
		}},
	},
	{
		id:   "build/complete_in_the_saves_turn",
		home: site{file: buildFrame, max: 1},
		why:  "A complete rides the SAME turn as the save that built its deliverable; the renderChecklist seed must not restate it.",
		// Anchored on the tool call plus the timing claim - the rule IS the pairing, so
		// neither half alone states it.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "complete-call", variants: []string{"manage_tasks", "`complete`", "completions"}},
			{name: "same-turn", variants: []string{"same turn", "in the turn", "free no-body rider"}},
		}},
	},
	// This rule's home is the always-on mental model, not the plan frame: the mental
	// model carries the whole "## The platform IS the server" section, and the plan
	// frame's line is only a pointer ("see your mental model"). pack.Embedded() cannot
	// see mental-model.md, so the real home is recorded here explicitly.
	{
		id:       "platform/is_the_server",
		home:     site{file: mentalModel, max: 1, note: "The always-on mental model owns the platform's shape; it rides every prompt."},
		pointers: []site{{file: planFrame, max: 1, note: "\"The platform IS the server (see your mental model)\" - names the rule, defers the body."}},
		why:      "The platform serves every page, route and asset; no backend is authored.",
		// Anchored: the claim plus one of the concrete artifacts it rules out. The
		// plan frame's pointer names the rule and stops, so it hits one class only.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "platform-serves", variants: []string{"platform is the server", "platform serves every"}},
			{name: "no-backend-artifact", variants: []string{
				"/server.go", "`/static/`", "upgrade handler", "never author a backend", "no backend",
			}},
		}},
	},
	{
		id:   "platform/module_scope_isolated",
		home: site{file: "knowledge/contracts/module-scope.contract.md", max: 1, note: "The module-scope contract owns the law; mental-model.md transcludes it under Platform laws (PlatformLawIDs reads only contract tokens) and the write gate enforces it in every stage."},
		why: "Every project JS file is an isolated ES module: a name a file neither declares nor " +
			"imports throws at runtime, whatever a sibling defines. Stated once where every stage " +
			"reads it, so LIVE edits get the law without recalling a guide.",
		// Anchored on the scope claim plus the runtime throw it predicts; a pointer
		// names the rule without asserting both halves.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "isolated-scope", variants: []string{
				"isolated es module", "isolated scope", "nothing you define is ambient", "not \"ambient\"",
			}},
			{name: "undefined-throw", variants: []string{"is not defined"}},
		}},
	},
	{
		id:   "plan/plaintext_drop_enforcement",
		home: site{file: planFrame, max: 1},
		why:  "The security posture for a plaintext drop is a PLAN-time architecture judgment.",
		sig:  signature{kind: detectPhrase, all: []class{{name: "phrase", variants: []string{"plaintext-drop enforcement"}}}},
	},
	{
		id:   "plan/site_pages_vs_app_views",
		home: site{file: planFrame, max: 1},
		why:  "Site-pages-with-routes vs app-views-of-one-page is an architecture choice PLAN owns; BUILD receives it as a locked decision.",
		sig:  signature{kind: detectPhrase, all: []class{{name: "phrase", variants: []string{"its screens are client-side views"}}}},
	},
	{
		id:   "guides/split_import_tree_shape",
		home: site{file: splitGuide, max: 0},
		why:  "The one-directional import tree is the client_js_split guide's rule; frames point at it.",
		sig:  signature{kind: detectPhrase, all: []class{{name: "phrase", variants: []string{"one-directional tree"}}}},
	},
	// ---- rules whose home is the project_state snapshot ------------------------
	// Both of these describe a section of project_state, sit directly beside the
	// data they explain, and ride EVERY build turn. A guide or frame that restates
	// them pays for the same words a second time in a place the model reads less
	// often, so the guide keeps a pointer and the snapshot keeps the mechanism.
	{
		id:   "snapshot/bind_to_declared_dom",
		home: site{file: snapshotRender, max: 1, note: "the Page DOM anchors block explains the list it sits above."},
		why: "Bind to the ids the page markup actually declares; a getElementById for a container " +
			"the page never declares reads null and throws behind an interaction the load probe " +
			"never triggers.",
		sig: signature{
			kind: detectAnchored, // anchored on the DOM lookup API plus the invented-container claim
			all: []class{
				{name: "dom-lookup", variants: []string{"getelementbyid", "queryselector"}},
				{name: "invented-container", variants: []string{
					"invent a container", "container the page", "page doesn't declare", "page does not declare",
					"never declares", "page dom anchors",
				}},
			},
		},
	},
	{
		id:   "snapshot/shared_shape_defined_once",
		home: site{file: snapshotRender, max: 1, note: "the Module contracts block explains the shapes: line it renders."},
		why: "Define a shared payload's field keys ONCE and read/write exactly those keys; a producer " +
			"and consumer that disagree on a key pass every symbol check and fail only at runtime.",
		sig: signature{
			kind: detectAnchored, // anchored on the shapes: line plus the no-gate-catches-it claim
			all: []class{
				{name: "shape-surface", variants: []string{"shapes:", "shape once", "shape ONCE", "field keys"}},
				{name: "silent-failure", variants: []string{
					"no gate", "nothing checks it", "only at runtime", "fail only at runtime",
					"misbehaves at runtime", "passes every", "pass every",
				}},
			},
		},
	},
	{
		id:   "guides/split_asset_scope",
		home: site{file: splitGuide, max: 1},
		why: "`scope` picks which pages carry a file's TAG and nothing else - an ES module's " +
			"load order comes from the import graph, so there is no tag order to tune. The build " +
			"frame carried a copy that both restated this and pointed at the guide for the details.",
		// Anchored on the arg value the rule is about plus the load-order claim that makes it
		// a rule; a pointer names `scope` without asserting the load-order half.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "scope-values", variants: []string{"scope=global", "scope=page", "`scope`"}},
			{name: "no-load-order", variants: []string{
				"load order", "load-order", "not tag order", "tag order",
			}},
		}},
	},
	{
		id: "plan/task_path_ownership",
		// max 2: the `paths` bullet and its own "never list builder-owned filenames"
		// sub-bullet are one authored region the block-walk sees as two.
		home: site{file: planFrame, max: 2},
		why: "Which artifacts a task OWNS - routes/tables/resources, never a builder-chosen " +
			"filename - is a PLAN-time rule. plan_schema.go's `paths` description carried a " +
			"673-byte paraphrase of it, pinned onto the SAME request as the frame.",
		sig: signature{kind: detectAnchored, all: []class{
			{name: "owns", variants: []string{"owns", "own the same", "task owns"}},
			{name: "not-filenames", variants: []string{
				"builder-owned filename", "builder-independent", "never the internal",
				"let the build agent own", "never \"/files/",
			}},
		}},
	},
	// NOT registered: the 'auth' helper ROSTER. manage_auth's WhenToUse and the create
	// result each carried their own five-name list where the auth-window-api law names
	// seven - a contradiction, and both arrived after the law. Both copies are gone. But no
	// signature here can separate a roster ("these are the helpers") from a legitimate
	// USAGE example, which the contract and the auth_contract guide are full of and which
	// must keep naming the one or two helpers they teach. Every anchor tried fired on those.
	// A guard that needs an allowlist of the sites it should watch is worse than none, so
	// this rule stays enforced by review - one of the paraphrase cases AUTHORING.md is
	// candid that no deterministic test decides.
	{
		id:   "guides/split_default_one_file_per_page",
		home: site{file: splitGuide, max: 1},
		why:  "The split default lives in the guide; the build frame's split section is a pointer, never a restatement.",
		// Anchored on the artifact kind, which is the platform noun the rule is about
		// and which a paraphrase cannot drop, plus the per-page default it states.
		sig: signature{kind: detectAnchored, all: []class{
			{name: "file-artifact", variants: []string{"`kind=file`", "kind=file", "linked js file"}},
			{name: "one-per-page-default", variants: []string{"one linked", "per page", "one file per page"}},
		}},
	},
}

// ---- surfaces ---------------------------------------------------------------

// promptGoFiles are the Go files whose string literals reach a model prompt, traced
// from the spec.System / llm.Message call sites. engine/title.go is deliberately
// excluded: its one-shot title/reaction calls carry none of the build frame, so
// single-home has nothing to say about them.
var promptGoFiles = []string{
	"stage_build.go",
	"stage_live.go",
	"stage_plan.go",
	"stage_expand.go",
	// build_seed.go holds the fresh-build instruction, the rendered checklist,
	// the shared seed, and the intent block - ~24KB of prompt-facing prose that
	// reaches the model on every build seed and reseed.
	"build_seed.go",
	// plan_schema.go's field descriptions are a second LLM-facing surface: a
	// grammar-constrained provider reads them at emission time. They paraphrase the
	// plan frame, so they belong under single-home like the frame itself.
	"plan_schema.go",
	"session.go",
	"guards.go",
	"snapshotbuild/snapshot.go",
	// render.go is where every word of project_state's guidance actually lives, and
	// project_state rides EVERY build turn - the highest-frequency LLM text in the
	// loop. Listing only snapshot.go left it the least-guarded surface in the repo.
	"snapshotbuild/render.go",
	// plan_coherence.go's six feedback messages are a full BUILD-frame surface: each
	// is handed straight to the planner as a corrective turn, so a rule restated there
	// competes with the frame in the same conversation.
	"plan_coherence.go",
	// guide_prefetch.go carries the PLAN guide-picker's whole system prompt plus the
	// capability-gap notes, both authored prose that reaches a model.
	"guide_prefetch.go",
	// Outside engine/, so relative. pack/assemble.go holds the guide-index lead-ins and
	// the tool-catalogue preamble - the wrappers around every catalogue in the prompt,
	// and the surface where the find_tool and guide-recall protocols kept reappearing.
	"../pack/assemble.go",
}

type block struct {
	file string
	line int
	text string // normalized
	raw  string
}

var bulletRe = regexp.MustCompile(`^([-*]|\d+\.)\s`)

// splitMarkdownBlocks splits a body into the unit an AUTHOR writes - a bullet, a
// numbered item, a paragraph, or one fenced snippet - which is the window the
// conjunction is scored over.
//
// Bullet-awareness is NOT optional: _common_rules has no blank lines between its
// bullets, so a blank-line-only splitter collapses the whole ground-rules list into
// one block and every rule appears to co-occur with every other.
//
// Fenced code IS scanned (unlike candidateProseLines, which skips it because snippets
// legitimately repeat across guides). This guard anchors on identifiers, and the facts
// most worth anchoring - a version pin, an import specifier - live only inside fences.
// A fence is flushed as its own block so its tokens cannot co-occur with the prose
// around it.
func splitMarkdownBlocks(file, body string) []block {
	var out []block
	var cur []string
	curLine := 0
	inCode := false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		raw := strings.Join(cur, " ")
		if n := contracts.Normalize(raw); strings.TrimSpace(n) != "" {
			out = append(out, block{file: file, line: curLine, text: n, raw: raw})
		}
		cur = nil
	}
	for i, rawLine := range strings.Split(body, "\n") {
		if codeFence(rawLine) { // shared with pack_duplication_test.go
			inCode = !inCode
			flush()
			continue
		}
		t := strings.TrimSpace(rawLine)
		if t == "" {
			if !inCode {
				flush()
			}
			continue
		}
		if !inCode {
			if strings.HasPrefix(t, "|") { // table row
				continue
			}
			if bulletRe.MatchString(t) || strings.HasPrefix(t, "#") {
				flush()
			}
		}
		if len(cur) == 0 {
			curLine = i + 1
		}
		cur = append(cur, t)
	}
	flush()
	return out
}

// packBlocks walks the embedded knowledge tree. Deliberately NOT pack.Embedded():
// pack.Pack carries no source path (pack/pack.go), so it cannot produce the file:line
// this guard must print, and its Body is front-matter-stripped and sidecar-joined.
func packBlocks(t *testing.T) []block {
	t.Helper()
	var out []block
	err := fs.WalkDir(knowledge.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		// AUTHORING.md is the doctrine that NAMES the rules - its "Where a rule lives"
		// table quotes every one on purpose. pack.ParseFS never sees it (the basename
		// isn't STAGE.md/GUIDE.md), so today's guards are only ACCIDENTALLY safe from
		// it; this walker is not. Excluded explicitly, not by luck.
		if strings.HasSuffix(p, "/AUTHORING.md") || p == "AUTHORING.md" {
			return nil
		}
		b, err := fs.ReadFile(knowledge.FS, p)
		if err != nil {
			return err
		}
		out = append(out, splitMarkdownBlocks("knowledge/"+p, string(b))...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk knowledge.FS: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("only %d knowledge blocks - did the tree move? the guard would pass vacuously", len(out))
	}
	return out
}

// toolProseBlocks yields every registered tool's LLM-facing prose: OneLiner,
// WhenToUse, each action's summary, and each arg description.
//
// This surface was outside the guard entirely, and it is neither small nor quiet: the
// core+backbone set alone has its own byte budget (TestAlwaysLoadedRoutingProseBudget)
// because it rides every build's cached prefix, and a plan-declared tool's WhenToUse
// rides the build seed. A rule stated in a frame AND in an arg description is the same
// two-wordings-read-as-two-rules failure the doctrine exists to stop, and until now
// only review could see it - which is exactly how the auth-helper roster came to name
// five helpers where the law names seven.
//
// Read from the REGISTRY rather than parsed out of tools/*.go: the registry is what
// actually reaches a model, so a string that never makes it into a ToolDef is correctly
// invisible here, and no file list can fall out of date.
//
// Blocks are keyed "tools/<name>" - one home per TOOL, not per field, matching how a
// site is declared (file + max).
func toolProseBlocks(t *testing.T) []block {
	t.Helper()
	var out []block
	reg := tools.DefaultRegistry()
	defs := reg.List()
	add := func(name, text string) {
		if nrm := contracts.Normalize(text); strings.TrimSpace(nrm) != "" {
			out = append(out, block{file: "tools/" + name, text: nrm, raw: text})
		}
	}
	for _, d := range defs {
		add(d.Name, d.OneLiner)
		add(d.Name, d.WhenToUse)
		actions := make([]string, 0, len(d.ActionMeta))
		for a := range d.ActionMeta {
			actions = append(actions, a)
		}
		sort.Strings(actions)
		for _, a := range actions {
			add(d.Name, d.ActionMeta[a].Summary)
		}
		for _, p := range d.Params {
			add(d.Name, p.Description)
		}
	}
	// Harness self-check, same shape as goPromptBlocks': a registry that came up empty
	// (an import dropped, a blank import removed) would make every rule below look
	// single-homed. Silent blindness is the one failure a ratchet cannot recover from.
	if len(defs) < 20 || len(out) < 200 {
		t.Fatalf("registry yielded %d tools and %d prose blocks - the guard is not seeing the tool surface", len(defs), len(out))
	}
	return out
}

// isRejectionCall reports whether a call constructs a failure result.
func isRejectionCall(fun ast.Expr) bool {
	name := ""
	switch f := fun.(type) {
	case *ast.Ident:
		name = f.Name
	case *ast.SelectorExpr:
		name = f.Sel.Name
	}
	switch name {
	case "Fail", "Failf", "FailCode", "Errorf", "Error":
		return true
	}
	return false
}

// hasFailureDiscriminant reports whether a composite literal is a rejection payload -
// map[string]any{"success": false, "code": ..., "error": ...} (guards.go) or
// &tools.Result{Success:false, Code:..., Error:...} / &Verdict{Block:true,...}.
func hasFailureDiscriminant(cl *ast.CompositeLit) bool {
	for _, e := range cl.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key := ""
		switch k := kv.Key.(type) {
		case *ast.Ident:
			key = k.Name
		case *ast.BasicLit:
			key, _ = strconv.Unquote(k.Value)
		}
		switch strings.ToLower(key) {
		case "code", "error", "block":
			return true
		}
	}
	return false
}

// isRejection reports whether the literal at the top of stack is part of a rejection
// message. AUTHORING.md's Non-goals REQUIRE a rejection to carry its rule inline: it is
// read at the point of the defect, in place of the frame, so it must be self-contained.
// That text is governed instead by AUTHORING.md's "one home per shared rejection clause"
// (store.PathParamNote &c) - a different assertion with its own guards.
//
// Concretely: guards.go's re-orient nudge contains "Re-read the last error" - the
// reread token, in a rejection, meaning the OPPOSITE of the rule. Flagging it would
// demand a regression. Rejection-shaped construction is the PROXY, not the doctrine;
// rule.known is the escape hatch when it misjudges.
func isRejection(stack []ast.Node) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		switch n := stack[i].(type) {
		case *ast.CallExpr:
			if isRejectionCall(n.Fun) {
				return true
			}
		case *ast.CompositeLit:
			if hasFailureDiscriminant(n) {
				return true
			}
		}
	}
	return false
}

// goPromptBlocks AST-walks the prompt-bound Go files and yields their frame-prose
// string literals. AST is required, not a nicety: a grep false-positives on Go COMMENTS
// (snapshot.go and session.go both discuss "ground truth" and "the re-read storm" in
// comments that never reach a model).
func goPromptBlocks(t *testing.T) []block {
	t.Helper()
	var out []block
	fset := token.NewFileSet()
	for _, f := range promptGoFiles {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		var stack []ast.Node
		ast.Inspect(af, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			stack = append(stack, n)
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if isRejection(stack) {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if nrm := contracts.Normalize(v); strings.TrimSpace(nrm) != "" {
				out = append(out, block{file: f, line: fset.Position(lit.Pos()).Line, text: nrm, raw: v})
			}
			return true
		})
	}
	// Harness self-check: a listed file that yields nothing is being scanned in name
	// only, and the registry above would go on reporting it as covered. That is how a
	// path typo or a file whose prose moved elsewhere turns into silent blindness.
	byFile := map[string]int{}
	for _, b := range out {
		byFile[b.file]++
	}
	for _, f := range promptGoFiles {
		if byFile[f] == 0 {
			t.Errorf("promptGoFiles lists %s but the walk found no prompt prose in it - "+
				"either the path is wrong or its LLM-facing strings moved. A listed-but-empty "+
				"file makes this guard report coverage it does not have.", f)
		}
	}
	return out
}

// ---- detector ---------------------------------------------------------------

// states reports whether one block STATES the rule: every class hits, no veto present.
// The conjunction is what buys the precision, and it is also what makes the guard
// pointer-tolerant for free: a pointer states the rule's NAME ("the ground rules cover
// which surfaces are current") and hits at most one class, while a restatement states
// its MECHANISM and hits all of them. A pointer cannot hit the full conjunction
// without becoming a restatement - which is exactly the line the doctrine draws.
func (s signature) states(b block) (matched []string, ok bool) {
	for _, v := range s.veto {
		if strings.Contains(b.text, contracts.Normalize(v)) {
			return nil, false
		}
	}
	for _, c := range s.all {
		hit := ""
		for _, v := range c.variants {
			if strings.Contains(b.text, contracts.Normalize(v)) {
				hit = v
				break
			}
		}
		if hit == "" {
			return nil, false
		}
		matched = append(matched, c.name+"("+hit+")")
	}
	return matched, true
}

func findSite(sites []site, file string) (site, bool) {
	for _, s := range sites {
		if s.file == file {
			return s, true
		}
	}
	return site{}, false
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func TestRuleSingleHome(t *testing.T) {
	blocks := append(append(packBlocks(t), goPromptBlocks(t)...), toolProseBlocks(t)...)

	for _, r := range ruleRegistry {
		hits := map[string][]block{}
		matches := map[string][]string{}
		for _, b := range blocks {
			if m, ok := r.sig.states(b); ok {
				hits[b.file] = append(hits[b.file], b)
				matches[b.file+":"+strconv.Itoa(b.line)] = m
			}
		}

		if len(hits[r.home.file]) == 0 {
			t.Errorf("rule %q: its home %s no longer states it.\n  rule: %s\n"+
				"  Either the rule was dropped, or it was rephrased past its anchors - reword the signature if the rephrase was intentional.",
				r.id, r.home.file, clip(r.why, 200))
		}

		declared := append([]site{r.home}, r.pointers...)
		declared = append(declared, r.known...)

		for file, bs := range hits {
			s, ok := findSite(declared, file)
			if !ok {
				for _, b := range bs {
					t.Errorf("rule %q is restated at %s:%d\n  matched: %s\n  block: %q\n  home: %s\n  rule: %s\n"+
						"  The home already rides this prompt. A second statement IN DIFFERENT WORDS reads to the model as a SECOND, DIFFERENT rule.\n"+
						"  Fix: cut to at most one pointer line (name the rule, don't restate its mechanism), or move the home in ruleRegistry (engine/rule_singlehome_test.go).",
						r.id, b.file, b.line, strings.Join(matches[b.file+":"+strconv.Itoa(b.line)], " + "), clip(b.raw, 160), r.home.file, clip(r.why, 200))
				}
				continue
			}
			if s.max > 0 && len(bs) > s.max {
				t.Errorf("rule %q: %s states it %d times, allowed %d.%s\n  Collapse the extras to one statement.",
					r.id, file, len(bs), s.max, noteSuffix(s.note))
			}
		}

		// A declared site that no longer matches must be removed: the ratchet turns one
		// way, so cleaned-up debt cannot linger in `known` and silently re-authorise a
		// future restatement.
		for _, s := range r.known {
			if len(hits[s.file]) == 0 {
				t.Errorf("rule %q: known-debt site %s no longer states it - delete that `known` entry so the ratchet holds.%s",
					r.id, s.file, noteSuffix(s.note))
			}
		}
	}
}

func noteSuffix(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return "\n  note: " + note
}
