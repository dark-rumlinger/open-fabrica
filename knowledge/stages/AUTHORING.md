# Authoring the LLM-facing guidance: one home per rule

This tree (`knowledge/`) plus the tool definitions and the save-time gates are how
the platform steers the build model. It grew by accretion: every build failure
tended to add a paragraph in a stage frame *and* sometimes a save gate *and*
sometimes a validator *and* a fix-pass message. The same rule ended up authored
three or four times, in slightly different words, and the copies drifted. That
redundancy, not the rules themselves, is what confused the model and dragged
output quality down.

The fix is a discipline: **every rule has exactly one home, and every other layer
references it rather than restating it.** Before you add guidance, find the rule's
home in the table below and put it *only* there.

## Where a rule lives

| Rule category | Single home | How other layers relate to it |
|---|---|---|
| **Code-enforced / deterministic** - anything a deterministic detector reports: a save-time write gate (JS/CSS syntax, named/dynamic imports, undefined refs, module cycles, layout `{{content}}`/`{{assets}}`/inline-style/late-globals, self-referencing & cross-origin asset tags, route-param syntax, homepage-at-`/`) **or** a post-build validator (`engine/validators`: dead links, raw sockets on gated endpoints, unscoped owner tables, a login page in the signed-in shell) | the **detector itself**; its rejection or issue message is the teaching | Do **not** pre-explain it in a frame. At most one pointer line ("the platform rejects a malformed save with a precise reason; fix from it"), and zero when the message already teaches the fix. Adding a validator should RETIRE a prose home, not sit alongside one. |
| **Wire fact** - REST envelope, `auth_token`, realtime frames, e2ee wire, payment flow, page/layout, routing, trigger events, webrtc, endpoint routes (upload/stream/export/import/feed suffixes) | a **`contracts/*.contract.md`**, transcluded with `{{contract:<id>}}` | Reference the contract; never paste a copy. `TestGovernedDocsMatchContracts` fails a drifted inline copy. Facts in `mental-model.md` ride every prompt already - don't restate them in a stage frame. |
| **Architecture judgment** (the model must *choose*) - persistence store, realtime model, auth strategy, data-ownership scope, rendering/terrain representation | **`stages/plan/STAGE.md`**, once | BUILD trusts the plan's decision (and, where the guide's `## Decision:` block gives a machine `check`, the conformance validator); it doesn't re-derive the choice. |
| **Build-loop behavior** - project_state is ground truth, patch-don't-resave, trust-your-writes, one-impl-per-file, don't name tools to the owner | **`stages/_common_rules/STAGE.md`**, once | Go instruction strings (e.g. `freshBuildInstruction`) must not restate these; the fragment already rides the build/live system prompt. The batch-every-independent-call rule is the exception: its home is the build frame's Act step (`stages/build/STAGE.md`). |
| **Scope / visual expansion** - scope-to-brief-size, name the defining capability, visual personality | **`stages/expand/STAGE.md`**, once | PLAN/BUILD consume the expanded brief; they don't re-run the expansion rules. |
| **Capability contract / how-to** - a specific feature's modes and footguns (realtime, auth, payments, 3D, client-JS split) | **one `guides/<category>/<slug>/GUIDE.md`** | A stage frame points at the guide ("the `client_js_split` guide, inlined below, has the shape"); it does not copy the mechanics. |

Guides carry two tiers of content, and BUILD's preamble frames them differently:
**wire shapes and platform API/tooling rules** (contract transclusions, exact
frame/request shapes, platform-managed files) are binding, reproduce exactly,
while **field notes** (traps, defaults, sizing heuristics distilled from failed
builds) are strong guidance the model may knowingly deviate from. Author
accordingly: reserve MUST-language for the wire-shape tier, and write field notes
as the trap they mark, not as law.

**A guide's home is a platform capability, never a genre, and a guide holds only the
two tiers above.** Cut generic technique a strong model already writes correctly
unprompted (a render loop, a resize handler, fBm noise, cube-sphere math, biome
colouring): keep the wire shape and the play-only trap, drop the tutorial around them.
The rendering trio (`3d_rendering` + `voxel_worlds` + `heightfield_terrain`) is the
CEILING for genre-shaped guides, kept only because their failure modes are invisible to
every gate and page-probe (silent sign inversions, `NaN` screens, fall-through-floor).
Do NOT add a per-genre guide, card games, tycoons, roguelikes, spreadsheets,
platformers. Chasing a guide per game/app/tool type is unbounded and dilutes the model's
own judgment; trust the plan + the model for genre, and spend guide prose only on what
THIS platform, and only this platform, can tell the build model.

## The test before you write a paragraph

> *Would a strong model, shown the precise gate rejection (or the contract, or the
> guide), fail to recover without this paragraph?*

If **no**, don't write it, the gate/contract/guide is the home. Write prose only
where the model must make a **choice it cannot infer from a rejection**
(architecture, scope, a security trade-off), and write it in that rule's one home.

## Guardrails that keep this true (run under `go test ./engine/`)

- **`TestNoCrossPackDuplication` / `TestNoContractRestatement`** (`engine/pack_duplication_test.go`)
  - the automatic backstop: ANY substantial prose line that appears verbatim in two
  packs, or any canonical contract line pasted into a pack that does not transclude
  it, fails here, no one has to remember a canary.
- **`TestDetectorCodeSingleHome`** (`engine/gate_code_singlehome_test.go`) - the
  detector guard. It greps save-time reject codes out of `tools/content` and issue
  categories out of `engine/validators`, then fails if a code is named in more than
  one pack, or in a pack that is not its declared `detectorPointerHome`. The code
  token survives paraphrase, so this catches a reworded restatement that the verbatim
  guard misses - but only where the pack NAMES the code. A rule restated in pure prose
  carries no token and belongs in the registry below.
- **`TestRuleSingleHome`** (`engine/rule_singlehome_test.go`) - the rule registry.
  Each entry declares one rule's `home`, its sanctioned `pointers`, and any `known`
  debt; the guard fails on a statement of that rule anywhere else. It scans the whole
  knowledge tree **plus** the Go string literals that reach a prompt (rejection
  messages are classified out - see the Non-goals below), so `mental-model.md` and
  `freshBuildInstruction` are both covered where the pack-only guards cannot see them.
  It scans `knowledge/`, `promptGoFiles`, and the **tool registry**. `promptGoFiles`
  includes **`snapshotbuild/render.go`** - `project_state`'s own prose, the
  highest-frequency LLM text in the loop, and therefore the right HOME for a rule
  about a section of the snapshot (a guide or frame that restates one pays for the
  same words again somewhere the model reads less often). The registry contributes
  every tool's `OneLiner`/`WhenToUse`/action summaries/arg descriptions, read from
  `tools.DefaultRegistry()` rather than parsed out of `tools/*.go`, so a string that
  never reaches a `ToolDef` is correctly invisible and no file list can fall stale.
  That surface sat outside the guard entirely until it was added, which is how a rule
  could be stated in a frame AND in an arg description with only review to catch it.
  Detection anchors on IDENTIFIERS (`project_state`, `get`, a version pin) rather than
  prose, requiring two token classes to co-occur in one authored block: that survives a
  **reworded** restatement, which the verbatim guard above cannot catch, and it is
  pointer-tolerant for free (a pointer names a rule; a restatement states its
  mechanism, and only the latter hits a full conjunction). Its limits are real and
  documented in the file: a paraphrase that drops both anchors passes, and no test sees
  a *contradiction*. Add a rule by adding one registry entry.
- **`TestGovernedDocsMatchContracts`** (`engine/contracts_consistency_test.go`) - a
  governed fact must be transcluded or carry the contract's probe tokens; a drifted
  inline copy fails.
- **`TestDocsReferenceRealTools` / `…RealActions`** (`engine/docs_consistency_test.go`)
  - every tool/`action=` a frame names must exist in the registry.
- **`TestStageFrameSizeBudget`** (`pack/promptsize_test.go`) - caps each stage frame in
  bytes. This is the only guard a rewording cannot evade: `TestRuleSingleHome` reads
  the words and so rewards a thesaurus, while a byte budget never reads them and prices
  a restatement whatever it is called. Over budget means find the rule's one home and
  cut the copy; raising the cap is the last resort. Lower the budget when a cut lands -
  the test prints when one has drifted loose.
- **`TestGuideCoreTotalBudget`** (`pack/guide_coresize_test.go`) - the same kind of
  byte cap over the rendered capability cores. The frame budget above cannot see them
  (it empties `{{GUIDES_CATALOGUE}}`), so without this a rule collapsed out of a frame
  could reappear in a guide for free.
- **`TestPlanFacetCarriesEveryChoiceBlock` / `TestPlanFacetOmitsBuildMechanics`**
  (`pack/guide_decision_test.go`) - PLAN carries no tools and emits JSON, so it gets a
  guide's `## Decision:`/`## Choose:` blocks and its section titles, never the build
  mechanics. The first guard fails if a choice block is dropped (a planner that never
  sees one simply omits the decision); the second fails if the facet regresses to a
  full-core render.
- **`TestNoDecisionInExtended`** (`pack/guide_coresize_test.go`) - a guide's
  `## Decision:` block must stay in the core (above `<!-- extended -->`), or
  core-only inlining would drop it from the PLAN prompt.
- **`TestGuideSummariesAreRoutable`** (`knowledge/knowledge_test.go`) - a guide
  summary must be substantive and not echo its title; it is the only line the PLAN
  guide picker sees.

Further drift guards enforcing the same one-home / no-drift discipline (run under
`go test ./engine/ ./pack/ ./knowledge/`):

- **Guide front-matter & routing** (`engine/guide_frontmatter_drift_test.go`) -
  `TestGuideFrontMatterToolsAreRegistered`, `…RequiresResolve`, `…MapsRoundTrip`,
  `TestStructuralSignalsAreClaimed`: every `tools:`/`requires:` entry resolves, every
  declared entry routes back to its guide through the derived
  `guideToolMap`/`guideLibraryMap` (which are many-to-many - guides may share a tool),
  and every structural signal the engine raises is claimed by a guide and vice versa.
  Plus `TestGuideDecisionsAreUnambiguous` and `TestRequiredBaseSurvivesExclusion`
  (`engine/guide_decision_signal_test.go`).
- **Front-matter keys** - `TestGuideFrontMatterKeysAreKnown` (`knowledge/knowledge_test.go`)
  rejects any key `pack.Parse` has no case for, since an unrecognised key is dropped in
  silence and reads as working routing metadata; `TestKnownFrontMatterKeysAllParse`
  (`pack/pack_test.go`) keeps the exported allowlist from claiming a key Parse ignores.
- **`TestNoDanglingGuideRecallReferences`** (`engine/guide_reference_test.go`) - every
  `slug=<x>` recall pointer in any `.go`/`.md` resolves to an embedded guide.
- **Runtime ↔ docs** - `TestReducerRuntimeMatchesCanonicalSurface`,
  `TestReducerSurfaceDocumented`, `TestRuntimeFrameKeysDocumentedAndEmitted`
  (`engine/runtime_frames_drift_test.go`): the reducer `ctx` surface and every client-facing
  WS frame key are both emitted by the runtime AND documented in the knowledge tree.
- **`TestTriggerEventsContractCoversProjectFacingEvents`** (`engine/trigger_events_drift_test.go`)
  - every project-facing event constant is documented in the `trigger-events` contract.
- **`TestDocsCiteRealTestNames`** (`engine/drift_guards_test.go`) - every test a doc or a
  Go comment names by identifier still exists. Naming the test IS how a reader finds the
  enforcement behind a rule, so a renamed test leaves the citation reading as working
  provenance while pointing at nothing. Go comments are in scope because that is where the
  costliest citations live: a helper documented as kept in step by a guard nobody wrote,
  and a doc comment headed by the name its function used to have. A Test-prefixed name
  that is a real func/method/type declaration is subtracted rather than allowlisted, so an
  HTTP handler whose name happens to start with Test is documented, not flagged.
- **`drift_guards_test.go`** - `TestReadmeEnvDefaultsMatchConfig` (README env table matches
  `config/defaults.go`), `TestPackContractTokensResolve` (every `{{contract:id}}` resolves),
  `TestPlanSecurityContractResolves`, `TestBriefImpliesEncryptionAccountVeto`.
- **Knowledge front-matter & index** - `TestGuidesHaveRequiredFrontMatter`,
  `TestMentalModelContractsResolve`, `TestEveryGuideAppearsInAssembledIndex`
  (`knowledge/knowledge_test.go`); `TestNoEmptyGuideCores`, `TestGuideCoreNotOversized`
  (`pack/guide_coresize_test.go`).

## Adding a capability guide - checklist

**A guide's routing lives in its own front-matter, never in Go.** The PLAN picker's
system prompt carries only generic rules; every per-guide rule reaches it from the
keys below. Adding a guide is a one-file change, and no routing table anywhere names
a slug.

1. `knowledge/guides/<category>/<slug>/GUIDE.md` with front-matter: `slug`,
   `role: capability`, `title`, `summary` (say the capability *and when to reach for
   it* - `TestGuideSummariesAreRoutable`), `category`, plus the routing metadata it
   needs. Only keys `pack.Parse` knows are allowed - an invented one is dropped in
   silence, so `TestGuideFrontMatterKeysAreKnown` rejects it:
   - `use_when:` / `not_when:` - one line each, addressed to the PLAN picker. What
     counts as evidence for this capability, and what it is *not* for. This is where
     an anti-example belongs ("diggable ground alone is not a cube world").
   - `evidence_gated: true` - for a guide with a large body and a clear positive
     signal, so the picker's include-when-unsure bias does not tax every plan with
     it. A gated guide MUST declare both `use_when` and `not_when`.
   - `exclusive_with:` - guides describing a RIVAL architecture for the same thing.
     At most one member of a group survives a pick; `exclusive_default: true` marks
     the safer member that wins when the evidence ties.
   - `tools:` - the tools whose use implies the guide, the single home of the
     tool→guide link. Many-to-many: guides may share a tool, so one large capability
     can be split into per-model guides.
   - `libraries:` - library specifiers that imply the guide. `requires:` - base
     guides. `decisions:` - `<topic>/<choice>` pairs whose being locked implies it
     (for a guide with no tool or library). `structural:` - plan-shape signals the
     engine raises (`client_split`).
2. Put wire shapes and any `## Decision:` block ABOVE `<!-- extended -->` (the core
   that rides the prompt); push situational field notes below it. Transclude
   load-bearing facts with `{{contract:<id>}}` - never restate them.
3. A new category needs a one-line `knowledge/guides/<category>/CATEGORY.md`.
4. Don't restate the guide's rules in a stage frame; point at the guide.
5. `go test ./knowledge/ ./pack/ ./engine/` - the front-matter, index, size,
   decision-placement, and duplication guards all run here.

The tool side of this doctrine (OneLiner/WhenToUse, the tool→guide link) is
`tools/AUTHORING.md`.

## Direction (not yet built)

Routing is now derived: guide front-matter is the canonical capability registry, and
every routing table (`guideToolMap`, `guideLibraryMap`, `guideDecisionMap`,
`guideStructuralMap`, `guideExclusiveMap`) is computed from it at load, so no
hand-maintained list names a guide. What remains is to generate the *human-readable*
indexes the same way - a `//go:generate` step with a clean-tree diff check - rather
than keeping them in prose that can quietly fall behind.

The rule registry has absorbed the old curated canaries, but most arrived as
`detectPhrase` entries - a single literal phrase, no better than the canary they
replaced, and blind to any rewording. What buys paraphrase resistance is giving a rule
an IDENTIFIER anchor and promoting it to `detectAnchored`. Promote one entry each time
you touch its rule.

## Non-goals

The validators and their **messages** are *not* redundancy with the frames. The
validator message is the *specific* defect and its concrete fix, read at the point
of the repair (a tool rejection, or the go-live report), so it must stay
self-contained: keep the finding + the fix inline rather than leaning on the model
to cross-reference the frame. That specific rejection is what this doctrine tells
the frames to defer to, not duplicate.

## Validation code: one home per shared rejection clause

The doctrine above governs *prose* (packs, contracts, tool descriptions) and its
drift tests scan those texts. It does **not** see the Go that renders a gate
rejection, and that is where the same failure mode crept back in. One defect is
often described by several functions: the save-time write gate, the `check` action,
and the post-build validator each render the same `store.Issue`/`store.ImportIssue`;
several distinct gates each teach the same platform rule (how an asset loads, how a
path parameter is spelled, that a required arg is missing). When each writes its own
copy of the shared clause, the copies drift, and the model sees the *same* mistake
explained differently depending on which surface caught it, the exact whipsaw this
doctrine exists to kill.

So the same rule applies to the code: **a remediation clause shared by more than one
rejection has exactly one home, a `store` constant or helper, and every surface
references it.** This does *not* soften the Non-goal above: each message stays
self-contained *at runtime* (it still carries the whole clause inline); single-homing
only stops the *source* from holding N drifting copies. A case-specific fast fix (an
`@import` fallback, a URL-filled `add_library` call, a did-you-mean path) still leads
each message; the shared note is the common frame it sits in.

Single-homed clauses today, each with a guard that fails if a surface stops
referencing it or the clause is gutted:

- `store.PathParamNote` - page-route `:param` vs literal endpoint paths, mirrors
  `routing.contract.md`; used by `validatePagePath` (store) and `ValidatePathSegments`
  (tools). Guards: `TestPagePathParamRejectionsShareTheNote`,
  `TestEndpointPathRejectionCarriesSharedNote`.
- `store.AssetLoadingNote` - the canonical asset channels; used by the cross-origin
  and same-origin asset gates. Guard: `TestAssetRejectionsShareTheChannelNote`.
- `tools.CodeMissingArg` - the one `missing_arg` code across the schema check and the
  `RequireString`/`RequireAction` helpers. Guard: `TestRequireHelpersCarryMissingArgCode`.
- `store.HumanSize` - size ceilings derive from the byte constants, never hand-typed
  KB prose. Guard: `TestBodySizeProseMatchesConstants`.

Direction: the triplicated `store.Issue` renderers (save gate / `check` /
post-build validator) collapse onto one `store`-level explainer per category, each
surface appending only its own context, the same shared-core / per-surface-extension
shape as the notes above.
