# Eval

`openfabrica eval` builds a fixed corpus of briefs on a running server and scores each build, so a change to the prompts, the model settings or the checks can be compared before and after on the same model. [Triage](TRIAGE.md) says what a build cost and where it churned; eval says whether what it built is what was asked for.

## Run it

Start the server as usual (the build needs the live public server, because the probes load and click real pages), then from the repo root:

```bash
openfabrica eval
```

| Flag | Default | Meaning |
|---|---|---|
| `--briefs <glob>` | `testdata/eval/briefs/*.md` | The corpus. One brief per file; optional front matter `---\nambition: high\n---`. |
| `--ambition` | `medium` | Ambition for briefs that set none. |
| `--model-id <n>` | system default | The `llm_model_id` the projects build with. |
| `--user-flows` | off | Turn user-flow tests on for the projects. |
| `--parallel <n>` | `1` | Builds at once. Keep it at 1 for numbers you compare: parallel builds share one provider. |
| `--timeout <d>` | `90m` | Give up on one build after this long. |
| `--out <dir>` | `eval-results` | Each run writes `<out>/<timestamp>/`. |
| `--server <url>` | `http://127.0.0.1:<admin port>` | The admin API. |

The command signs in as the instance's admin with the instance's own signing key (it runs on the machine that hosts the server, like `reset-admin`). Planning questions are answered "decide for me"; a secret question is deferred. The projects are kept, so every build can be opened afterwards.

## Read it

Each brief writes `<brief>.json`; `summary.json` holds them all plus totals. The columns:

| Field | Means |
|---|---|
| `acceptance_failures` | The headline. Final-sweep errors a user meets (the click-through's dead, throwing, covered and overflowing controls, blank pages, runtime errors, `spec_gap`) plus every planned item left unbuilt. Lower is better. |
| `spec_met` / `spec_partial` / `spec_missing` | The spec audit's latest verdict per checklist item. `spec_met_ratio` in the totals is over every audited item in the corpus. |
| `features_uncovered` | Brief features the plan neither covered nor excluded. |
| `unfinished` | Planned items not done. Repair items are counted separately as `repairs_done`. |
| `errors` / `warnings` | The final validator sweep by category. |
| `output_tokens`, `billed_input_tokens`, `duration_sec` | What the build cost. |

## Compare two runs

```bash
openfabrica eval compare eval-results/<before> eval-results/<after>
```

Prints B minus A per brief (only the briefs both runs share) and in total, plus the billed-input ratio. A change ships when acceptance failures and unmet spec items go down and billed input stays within budget. One run per side is noisy: run a corpus twice before trusting a small delta.

`openfabrica eval collect <project-id>...` re-reads finished projects without building anything, for example ones built by hand.
