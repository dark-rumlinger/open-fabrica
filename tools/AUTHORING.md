# Authoring a tool: one home per fact

Tools live under `tools/<category>/`, each registered with
`tools.Define(tools.Tool{...})`. This side of the platform obeys the same rule as
the knowledge tree (`knowledge/stages/AUTHORING.md`): **every fact has exactly one
home, and every other layer references it rather than restating it.** The build
model reads a lot of tool text, the wire `description`, the tool catalogue,
`find_tool` docs, and near-duplicate copies that drift in wording are what drag
output quality down. Before you write a sentence, find its home below and put it
*only* there.

## Where each fact lives

| Fact | Single home | How other layers relate to it |
|---|---|---|
| **Tool name** | `tools.Define(Name: …)` | Guides link to it by name in their `tools:` front-matter; `guideToolMap` DERIVES the tool→guide map from that, never hand-maintained. |
| **What the tool does** (≤130 chars) | `OneLiner` | Rendered verbatim as the LLM tool `description` (wire schema) and the catalogue line. Do **not** restate it in a guide body - reference the tool by name. |
| **When to reach for it over a neighbour** | `WhenToUse` | Reaches the model three ways, never the wire schema: `find_tool` returns it when the model loads a deferred tool, `Registry.RoutingProse` renders it for the always-loaded set on the BUILD/LIVE system prefix, and the build's intent block renders it for the tools the PLAN declared. Keep an explicit "prefer X over Y" contrast - it is what the ranker and the model route on. |
| **Per-action summary / example** | the Action struct's `Doc()` / `Example()` | Extracted by reflection at registration; the catalogue lists each `action` verb from here. |
| **A load-bearing wire fact the tool implies** (REST envelope, `auth_token`, realtime frames, payment flow) | a `contracts/*.contract.md`, transcluded `{{contract:<id>}}` into the owning guide | Never paste the fact into `OneLiner`/`WhenToUse` or a guide body. A drifted copy in a guide/pack fails `TestGovernedDocsMatchContracts` + `TestNoContractRestatement`; a copy in tool prose fails `TestNoContractRestatementInToolProse`. Reference a contract id in Go through a `contracts.ID*` constant, never a bare string. |
| **Which guide a tool implies** | the guide's `tools:` front-matter | Build-time auto-prefetch reads it; the tool declares nothing about guides. |
| **Brief-level routing** | the owning guide's `use_when:` / `not_when:` front-matter | Prose addressed to the PLAN guide picker, which is a model call, not a keyword match. Broad terms ("user", "live", "chat") belong here, where judgment reads them in context. |
| **Narrow per-task preload keywords** | `taskToolInference` (`engine/build_tools.go`) | The ONLY routing-keyword list in the engine, and high-precision by design: fired per TASK to preload a schema, not per brief. `TestTaskInferenceToolsRouteToAGuide` keeps every inferred tool tied to a guide that carries its contract (or explicitly listed as guideless). |

## The test before you write tool prose

> Would a strong model, shown the tool's `OneLiner` plus the guide/contract it
> points at, still emit broken code without this sentence?

If **no**, don't write it. Spend `WhenToUse` prose only on the disambiguation the
model cannot infer, which of two similar tools, and when, not on mechanics a
strong model already gets right, and not on facts that have a contract or guide home.

## Adding a tool - checklist

1. `tools/<category>/<tool>.go`: `tools.Define(tools.Tool{Name, Category, OneLiner (≤130), WhenToUse, Actions: […]})`. The file is named after the tool and lives in its `Category` directory (`TestToolFileNameCategoryAgree`). Each Action struct declares its `arg:"…"` fields and **reads every one** in `Run` (`TestNoUnreadActionArgs`).
2. A new category directory goes in `tools/all/all.go`, the one import list the binary and tests load the catalogue from (`TestAllCategoriesImported` fails until it is). A tool in an existing category needs nothing here.
3. Declare build-loop traits on the tool itself rather than editing engine lists: `Probe: true` for a tool that only observes the running project, `RealtimeModel: "<model>"` for a realtime wire-model tool, `Stages`, `MaxDuration`, `RequiresCapability` as needed.
4. If a capability guide should imply this tool, add the tool name to that guide's `tools:` front-matter, the ONLY place that link lives.
5. Add a `taskToolInference` entry **only** when a task-text word should preload the tool and the planner may not declare it. The tool must still route to an owning guide (or be listed guideless): `TestTaskInferenceToolsRouteToAGuide` holds that.
6. Do **not** restate the tool's behaviour in any guide or stage frame; reference it by name.
7. Test through `tools/tooltest`: `tooltest.New(t)` runs calls through the production `Executor.Invoke` path against fully migrated databases (`h.MustOK`, `h.MustFail`, `h.DB()`); `tooltest.ProjectDB(t)` is the short form for driving a handler directly. Never hand-write an `of_*` table (`TestNoHandWrittenPlatformDDLInToolTests`).
8. `go test ./tools/... ./engine/...` - the drift guards below run here.

Every caller runs a tool through `Executor.Invoke` with the shared `tools.Env`, which owns the per-call deadline (`MaxDuration`, else `tools.DefaultToolTimeout`), panic recovery, and the `ToolContext` shape, so a tool behaves the same from the build loop, live chat, a scheduled job, or an event trigger.

## Guardrails (run under `go test ./tools/ ./engine/`)

- **`TestNoUnreadActionArgs`, arg-alias** (`tools/argdrift_test.go`, `argalias_test.go`) - every declared `arg` is read through the receiver; weak-model aliases (`path`→`file_path`) normalise.
- **`TestNoWrongListEnvelopeInToolProse`, `TestNoCapabilityNameUsedAsAction`** (`engine/tool_prose_drift_test.go`) - tool prose can't teach the wrong REST envelope or use a plan-capability name as an action verb.
- **`TestTaskToolInferenceReferencesRealTools`, `…ToolsRouteToAGuide`** (`engine/tool_inference_drift_test.go`) - the inference table's tools are real and each pulls a guide that carries its contract.
- **`TestDocsReferenceRealTools` / `…RealActions`** (`engine/docs_consistency_test.go`) - every tool/`action=` a doc names exists in the registry.
- **`TestGuideArgsBelongToDeclaredTools`** (`engine/guide_arg_drift_test.go`) - the ARG-level twin of the two above, and the one the platform's own shape makes necessary: a capability family is several near-identical tools, so a guide about one of them can teach a SIBLING's setting. An arg no action field claims is dropped in silence by `populateActionStruct`, so the call returns green and the setting never exists. Scoped to assignment-form (`arg=`/`arg:`) tokens whose owner is a same-`Category` tool the guide does not declare; its cross-category blind spot is documented in the file rather than papered over with an allowlist.
- **`TestToolProseReferencesRealTools` / `TestNoContractRestatementInToolProse`** (`engine/tool_prose_consistency_test.go`) - a tool's OWN prose (`OneLiner`/`WhenToUse`/action summaries/arg descs) names only registered tools, and its `OneLiner`/`WhenToUse` does not restate a canonical contract line. The tool-side twin of the two guards above, which scan only packs+contracts.
- **`TestNoContractRestatementInCreateResult`** (`tools/api/ws_create_usage_test.go`) - the same check for a tool RESULT, which none of the scanners above read. A result builder is the largest LLM-facing prose a tool has and it lands in the conversation permanently, so it gets its own restatement ratchet. **By the time a create result is read, the guide its tool implies is already in the system prompt** - so a frame shape restated there is paid for twice; point at it.
- **`TestBuildSystemPromptCarriesAlwaysLoadedRoutingProse`, `TestBuildSharedSeedCarriesPlanToolRoutingProse`** (`engine/tool_routing_delivery_test.go`) - a DELIVERY guard, not a content one. Every check above polices prose that has to actually arrive; a preloaded tool reaches the model as a schema whose description is its one-liner, so without these two the mandatory, drift-guarded `WhenToUse` of the core set and of every planner-declared tool reached nobody.
- **`TestRuleSingleHome`** (`engine/rule_singlehome_test.go`) - the knowledge tree's rule registry, reading this side too: every tool's `OneLiner`/`WhenToUse`/action summary/arg `desc` is walked out of `tools.DefaultRegistry()` and scored against the same signatures the stage frames are. A rule that has a home in a frame or a guide and is stated again in an arg description is the same two-wordings-read-as-two-rules failure, and this is the only guard that sees across that boundary - the restatement checks above scan packs and contracts, never the registry.
- **`TestNoBlobChunkTaughtAsChatEnvelope`** (`engine/tool_prose_drift_test.go`) - a named anti-pattern scanner over packs + tool prose + production Go. The restatement guards are copy-paste ratchets and go blind the moment a fact is reworded; when a specific wrong claim is costly (e.g. an e2ee blob chunk described as the chat envelope), pin THAT claim negatively and the rewording is caught too.
- **`docs_consistency_test.go` registration & arg-tag guards** - `TestNoToolRegistrationErrors` (every blank-imported tool package registers with no `init()` error), `TestToolRequiredDescMatchesTag` / `TestToolDefaultDescMatchesTag` / `TestToolCapDescMatchesTag` (an arg `desc` that says "Required" / "default X" / a numeric cap must agree with its `required` / `default:` / `max:` tag), and `TestDocsShorthandVerbsResolve` (every backticked `create_*`/`enable_*`/`set_*` shorthand maps to a real (tool, action); no shorthand is unmapped and no table row is stale).

The guide/contract/stage side of this same doctrine is `knowledge/stages/AUTHORING.md`.
