# Triage

Triage is a read-only sweep over build history that answers one question: **what went wrong in this build, and what did it cost?**

It exists because "every tool call succeeded" and "the build went well" are not the same statement. A run can rewrite its own conversation history every few turns, re-buy the whole prompt prefix at full price, ship a page that throws on load, and still report a clean tool log. Triage reports both halves.

## Quick start

The admin server listens on `:5734`. Triage is mounted at `/admin/api/triage`.

```bash
# Read one project's latest build as a report (the usual thing you want)
curl -s "http://127.0.0.1:5734/admin/api/triage?project=<id>&all=1&format=text"

# Rank every build across every project, worst first (JSON)
curl -s "http://127.0.0.1:5734/admin/api/triage"

# Drill into one run: the full build export, including chat_messages
curl -s "http://127.0.0.1:5734/admin/api/triage?raw=1&build=<run id>"
```

Triage never writes. It is safe to run against a live server at any time.

## Parameters

| Param | Meaning |
|---|---|
| `project=<id>` | Restrict to one project. |
| `build=<run id>` | Restrict to one run. |
| `limit=<n>` | Runs scanned per project, newest first. Default 40, max 500. |
| `all=1` | Include clean builds. Without it, only builds with findings are returned. |
| `format=text` | Render a report instead of JSON. Requires `project=` or `build=`. |
| `raw=1&build=X` | Return the full build export for run X instead of the scan. |

`format=text` refuses to run unnarrowed on purpose: a text dump of every run in every project reads as noise rather than an answer.

## Reading the report

```
<run id>  <project name>  (project <id>)  <model>
status=completed stage=live  12m15s  VERDICT: EXPENSIVE (score 28)

COST   billed_in=1.50M  out=43.1K  cache_hit=80%  calls=30  overhead=34.8x
       re-bought 139.6K of context that caching should have covered

FINDINGS
  cache_thrash   x5   history rewritten mid-run, re-billing the prefix at full price; worst drop 36KB at iteration 11 ~90.0K
  cold_miss      x1   call served nothing from cache, first at iteration 5 ~49.6K
  redundant_save x3   artifact fully rewritten rather than patched: app.chat.js, app.js, app.state.js
  reread         x3   artifact read back repeatedly after this run wrote it: app.state.js, app.hud.js, app.js
  batch_tasks    x6   6 tasks completed together at iteration 24, so per-task cost is an even split, not a measurement
  tool_reject    x1   write/tool reject [manage_files] (patch_anchor_not_found)

TIMELINE (calls that re-bought context)
  it5   build  fresh_in=49.6K   cache_read=0       COLD MISS
  it11  build  fresh_in=26.6K   cache_read=37.8K   PREFIX -36KB
```

### Verdict

One word, so you do not have to interpret the score. A shipped defect outranks any amount of spend.

| Verdict | Means |
|---|---|
| `broken` | The run failed, or a validator found an error. Something shipped broken. |
| `expensive` | Re-bought 100K+ tokens of context that caching should have covered. |
| `noisy` | A non-zero score, but nothing broke and nothing large was re-billed. |
| `clean` | Score 0. Findings may still be listed: the non-scoring kinds (healthy pre-ship catches, cleared probe reds, `batch_tasks`, `self_correction`) never dirty a verdict. |

### Cost

`billed_input_tokens` is what the provider charges for: fresh input, plus cache reads, plus cache writes. `overhead_ratio` is billed input per output token.

**A high `overhead_ratio` on its own is not a defect.** Cache reads are heavily discounted, so a run with 45x overhead and `rebilled_tokens=0` is fine: the cache did its job and the overhead is just the fixed cost of a large system prompt and tool set. There is nothing to fix there.

The number that matters is `rebilled_tokens` (plus `cold_miss_tokens`). Those are fresh, full-price tokens spent re-buying context the run already had. That is the actionable waste.

## Findings

Two lenses, disjoint by construction, so their scores add without double-counting. The error lens counts calls the tools rejected. The cost lens counts tokens and successful-but-wasteful actions.

### Error lens (a tool returned `success:false`)

| Type | Means |
|---|---|
| `tool_reject` | A call was rejected. Carries the tool and the reject code. **Healthy pre-ship catches** are the gate catching a real code defect the model then fixed - surfaced, but NOT scored (the immune system working is not build friction). The set is `healthyPreShipCodes` in `server/admin/triage.go`: `syntax_error`, `static_error`, `undefined_reference`, `unresolved_import`, `dynamic_import_unresolved`, `export_removed_in_use`, `entry_exports`, `cyclic_import`, `window_dead_global`, `window_global_bridge`, `reserved_module_name`, `colliding_module_name`, the size gates `module_too_large_client` and `oversized_body`, plus the page/layout scramble family `page_not_fragment`, `page_has_layout_token`, `layout_missing_content`, `layout_duplicate_content`, `layout_duplicate_assets`, `layout_unknown_placeholder`, `layout_placeholder_in_rawtext`, `layout_unclosed_rawtext`, plus `hand_written_asset_tag` (a `<script src>` / stylesheet `<link>` typed into a page or layout body instead of attach / `{{assets}}`). Everything else is friction and scores, including the patch rejects `patch_anchor_not_found`, `patch_anchor_consumed`, `patch_chained_hunk` (a hunk that matched only text an earlier hunk in the same call had just written) and `patch_noop_replace`. |
| `patch_loop` | Two or more failed edits to the same artifact: a stuck edit/repair loop. |
| `task_ordering` | `complete` rejected because a dependency was not done. |
| `probe_failure` | A model-invoked `manage_testing` call returned `success:false`. A red on a target later re-probed green (a route probed before it was built, then re-verified) is the model's own verify loop - surfaced, but NOT scored. Only **unresolved** reds score. |
| `flow_catch` | A replay of the project's `of_test.js` (`test_flows`, or a `write_flows` save, which replays at once) went red, and every flow it failed later passed: an app bug (or a stale script) caught before ship. A red is cleared only by later greens whose `passed` list names its failed flows, so named-flow runs count toward clearing it. Also cleared when the final validator sweep has no `flow` issue, which means the post-build replay was green; the detail then says how many cleared only that way. Surfaced, but NOT scored - it is the flow tests doing their job. Never counted as `probe_failure`. |
| `flow_failure` | A `test_flows` replay was still red at the end: some flow it failed never passed again, and the final sweep did not replay the flows green. A replay refused by the budget (`budget_exhausted`) or skipped succeeds without replaying anything, so it clears nothing. The same goes for a green of a suite rewritten to other (debug) flows. Scores like an unresolved probe. |
| `llm_error` | A provider call returned an error. |
| `build_failed` | The run ended `status=failed`. |

### Cost lens (nothing failed, tokens burned anyway)

| Type | Means | Where to look |
|---|---|---|
| `cache_thrash` | The stable prefix collapsed mid-segment and the next call re-bought a large block of fresh input. The history was rewritten. | History compaction, anything that mutates messages already sent. |
| `cold_miss` | A call mid-segment was served nothing from cache. | Provider cache TTL or eviction. |
| `turn_shredding` | BUILD turns that called a tool, produced almost nothing (<300 output tokens), and could have ridden the turn before: a lone test, `complete` or small save sent right after a turn whose calls were all successful saves/completes. A tiny turn that had to wait for a result is not counted (the session's first turn, a turn after a platform message or a failed call, one acting on what it just read or tested); the detail says how many of those there were. Reported from 3 avoidable turns up. Output is the measure, not the call count: tool arguments ARE output, so a turn declaring six tables costs ~700 tokens while one asking for a single endpoint costs ~50. Every round-trip re-reads the whole prefix and pays a fixed per-call latency, so a chain split one call per turn buys an ordering guarantee the engine already gives. Tokens are what the counted turns billed, scored at the cache rate. | The Act step's batching rule; a tool whose calls cannot be argued up front. |
| `redundant_save` | An artifact was fully rewritten rather than patched. Not counted: a save whose body is under 1500 bytes (a patch would cost about the same), and a re-save of an existing file right after its previous write was rejected (the recovery the reject asked for). | Guidance pushing full-file writes over patches. |
| `reread` | An artifact was read back repeatedly after this run wrote it. | The model paging its own output back into context, often because a read is paginated. |
| `batch_tasks` | Three or more tasks completed on one iteration, so per-task cost attribution degrades to an even split (`estimated:true` in the export). A measurement artifact, not build friction - surfaced, but NOT scored. | Task granularity, completion prompting. |
| `self_correction` | The model found a defect in work it had already written. Prose-matched, so informational only: it never contributes to the score. | Reported for context, not as a defect. |
| `flow_chase` | The replay loop of `of_test.js` ran long or went wrong: 6 or more replays, a replay refused by the budget, a script rewritten into diagnostics (a save that kept fewer than half the previous flows after a replay that got no closer to green), or app edits made after a budget refusal with no replay left to verify them. Carries the billed input and wall time from the first replay to the end of the run. A run that spent half its billed input (and at least 1M tokens) after the first replay reads `expensive`. | The red-flow guidance, the replay budget, how much a red replay shows. |
| `flow_fix_mix` | How the run's reds ended: cleared after an app edit (a real bug the flows caught) or after only a script re-save. Informational, never scored. It answers whether the flows paid off for this build. | Compare across builds before changing the flow guidance. |

### From the run log

| Type | Means |
|---|---|
| `validator_issue` | A validator's verdict on the shipped artifacts (runtime probe, broken reducer, undefined reference, bad module specifier), the explore click-through (`inert_click`, `click_error`, `covered_control`, `layout_overflow`, `blank_page`), the `style_drift` check (a page restyling itself instead of using the shared stylesheet), or a `spec_gap` from the spec audit (a checklist item the code does not deliver). These are what is left after the repair rounds; an error here makes the build `broken`. The audit's own per-item verdicts are in the run log under ref `spec_audit`. |
| `unbuilt_task` | A checklist item never reached done, so the go-live report told the owner this work is missing. Validators only inspect what was built, so unbuilt work is invisible to them; the checklist is the only record that it was owed. Reads CURRENT `of_tasks` status, so an item a later run finished no longer counts. |
| `stage_failure` | A stage ended `failed`, `paused` or `blocked`. `of_runs` carries only the run-wide status, never which stage stopped or why. |

## Scoring

```
errorScore = frictionRejects + patchLoops*5 + unresolvedProbes*2 + unresolvedFlows*2 + taskOrdering + llmErrors*2   (+10 if the run failed)
costScore  = rebilledTokens/10000 + coldMissTokens/10000 + shreddedTokens/100000
             + redundantSaveFiles*3 + rereadFiles*1
flowScore  = flowTokens/100000 + diagnosticRewrites*3 + blindEdits*1      (only when a flow_chase is reported)
score      = errorScore + costScore + flowScore + validatorErrors*4 + validatorWarnings*1 + unbuiltTasks*4
```

`flowScore` overlaps `turn_shredding` partly (shredded turns inside the flow phase count in both), which is deliberate: a long replay loop should outrank a build that only shredded turns.

The score counts only what actually cost the build. Deliberately excluded, because they are the model working correctly rather than build friction, and would otherwise stamp a clean build `noisy`: `frictionRejects` omits **healthy pre-ship catches** (a defect the gate caught then the model fixed); `unresolvedProbes` omits probe reds later cleared by a green probe of the same target (the gather→act→verify loop); `unresolvedFlows` likewise omits `test_flows` reds whose failed flows later passed, or that the final sweep replayed green (`flow_catch`); and `batch_tasks` (a per-task-cost measurement artifact) no longer scores at all. All three are still surfaced as findings - the raw counts stay visible in the report; only the score changed. Tokens convert to points at a fixed rate so a run's token count cannot swamp the ranking. Shredded turns re-read a CACHED prefix rather than re-buying it, so they convert at a tenth of the re-billed rate; without that discount one build's cache reads would outweigh every other signal. Weights and thresholds are named constants at the top of `server/admin/triage_detect.go`; tune them there.

## Gotchas

**Prefix stability is only defined within one session segment.** Every segment opens with `stable_prefix_bytes=0` and buys its context for the first time. Comparing a segment's first call against the previous segment's last call reports a collapse of the entire prior conversation that never happened. `detectCost` keys on `of_llm_calls.session_id` for exactly this reason. If you add a detector over these columns, do the same.

**`of_chat_messages` has no `run_id`.** The brain dialogue is reconstructed by time-windowing between `started_at` and `completed_at`. A run still in flight has an unbounded upper bound and will absorb later messages.

**A failed tool result has no `data`.** Every summarizer routes a failure through `tools.SummarizeError`, which emits `{success,error,code}` alone and clips the error text to 150 bytes. Anything you want to classify on must survive that. Rich failure detail comes from the validator issues in `of_run_log`, not from the tool result.

**A failed `test_flows` result keeps only its error.** The failed flow names are read out of `"N of M flows failed: a, b"`. Flow names may themselves contain `, `, so a red is cleared by matching whole passed names at list boundaries, never by splitting the list.

**Copied projects show no history.** `EphemeralResetStmts` clears `of_chat_messages`, `of_llm_calls` and `of_run_log` on project copy/export while preserving `of_runs`, so a copied project lists runs with zero findings.

## Access

Loopback-only and unauthenticated when the admin server is bound to loopback, matching the admin panel's own `127.0.0.1` trust boundary. When admin is exposed off loopback, triage moves behind the admin JWT instead, because a reverse proxy makes `RemoteAddr` a spoofable `127.0.0.1`.

## Code

| File | Holds |
|---|---|
| `server/admin/triage.go` | HTTP handler, report assembly, error lens, dialogue reconstruction. |
| `server/admin/triage_detect.go` | Cost lens: detectors, scoring, thresholds. |
| `server/admin/triage_flows.go` | Flow lens: replay-loop cost, diagnostic rewrites, blind edits, app-versus-script fix mix. |
| `server/admin/triage_text.go` | The `format=text` renderer. |
| `server/admin/triage_test.go` | Tests. |

Data sources, all per-project in `data/projects/<id>/project.db`: `of_runs`, `of_chat_messages`, `of_llm_calls`, `of_run_log`, `of_tasks`.

The sweep opens every project database, so per-run queries multiply. Keep the per-run count small - currently seven: the call walk and the run log load (the heavy two, covered by `idx_of_llm_calls_run` and `idx_of_run_log_run`), the tool-code walk, the brain-dialogue window, the unbuilt-checklist read, and two scalar lookups. The call walk counts a turn's tools with `json_array_length` in SQL so a build's file bodies never cross into the sweep. A sweep over dozens of projects runs in well under a second.
