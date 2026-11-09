---
slug: agent_endpoints
role: capability
title: Hosted agent endpoints (multi-step tool-using assistants)
summary: A POST /run endpoint that runs a bounded tool-calling loop over the app's own functions and data - the multi-step sibling of the LLM proxy.
use_when: The app needs an assistant that takes SEVERAL steps to answer - choosing which of the app's own functions or data reads to call, in a loop - rather than returning one chat reply or one-shot generation.
not_when: A plain chatbot, a single "generate X" call, or chat-over-your-docs (RAG) - use manage_llm_proxy. A fixed "when X happens, do Y" server action - use manage_triggers.
evidence_gated: true
category: ai
position: 27
tools: [manage_agent]
---

# Hosted agent endpoints (an assistant that DECIDES and ACTS over several steps)

`manage_agent` mints one endpoint, `POST /api/<path>/run`, backed by the project's
LLM. Unlike the single-shot proxy, the platform runs a loop: the model plans,
calls the tools you allowlisted, reads their results, and loops until it can
answer or a cap stops it. No provider client code, no key in the browser.

**The agent's tools are the app's OWN capabilities, allowlisted per endpoint - never the platform build tools.** Three sources:
- `functions` - a CSV of `manage_function` endpoint paths the model may call. Each
  reaches the model as the tool `fn_<path>` (path characters outside letters,
  digits, `_` and `-` become `_`, so `lookup/order` is `fn_lookup_order`). The
  model reads each function's `description` (set it, with the body expected); say in
  `system_prompt` when to use each, naming the `fn_` tools exactly.
  An agent with no tools can only talk.
- `allow_data_query` - exposes one read-only SELECT tool (`query_data`) over the
  project tables (no writes, no system tables, no PASSWORD/ENCRYPTED columns). The tool
  lists table names; describe columns in `system_prompt`. **Tables behind an owner- or
  tenant-scoped endpoint are withheld** from everyone but admins and project-key callers
  (the model writes the SQL, so it cannot be trusted to filter on the caller); expose
  that data through a `fn_<path>` function that filters on `req.user.id`.
- `allow_http_fetch` - exposes an SSRF-guarded outbound HTTP fetch tool
  (`http_fetch`) for public APIs and pages (internal/loopback addresses are refused).

Set `max_subagents` (with `max_depth`) to let the agent DELEGATE a self-contained
subtask to a fresh sub-agent via a built-in `spawn_subagent` tool - see below.

**Request (same shape as the LLM proxy):** `{messages:[{role,content}, ...]}` with
roles `user`/`assistant` ONLY and the LAST message a `user` turn. The system
prompt lives on the endpoint - do NOT prepend a system message.

**Response - one JSON body** (NOT the REST endpoints' envelope shapes; read with `await res.json()`):
`{ content, steps, stop_reason, subagents, model, usage }`. `content` is the agent's final
answer; `steps` is how many model turns it took; `stop_reason` is `done` (it
answered), `step_cap` (it kept calling tools past `max_steps`), or `budget` (the
day's allowance ran out mid-run; a request arriving after it is spent gets **429**).

**Call these routes DIRECTLY from the page - do NOT wrap the agent in server functions.**
The run, the live step stream (`?stream=1`), background runs (`?async=1`), their
polling (`GET /run?id=`), and per-endpoint budgeting are ALL built in. Building
`manage_function` handlers (a `start`/`poll`/`finish` layer, your own `runs` table,
a second budget) duplicates the runtime AND collides with `/api/<path>/run`. The
page fetches the agent routes itself with `fetch` (send the token via `apiFetch`),
reading a `?stream=1` response through a `ReadableStream` reader - **never
`EventSource`**, which is GET-only, so it cannot POST the messages or send the auth
token; a GET to `/run` without `?id=` is answered 400 with that guidance.

## Cost and safety - do this for every agent endpoint

An agent makes MANY model calls per request, so it is the most expensive surface
the platform exposes. Bound it:
- **`daily_token_budget`** caps the WHOLE run's tokens per UTC day, not one call. A
  run stops when the day's allowance is gone (`stop_reason: budget`). An
  unbudgeted public agent is a spend leak.
- **`max_steps`** bounds the loop so a confused model can't spin.
- **`requires_auth`** is ON by default (unlike the proxy). Keep it on for any agent
  that can write data or spend real tokens.
- **Sub-agents share the run's budget and count.** `max_subagents` caps total spawns
  and `max_depth` caps nesting, so delegation multiplies calls but never spend past
  `daily_token_budget`.

## Prefer a simpler tool when it fits

Reach for a neighbour when the job is not multi-step: `manage_llm_proxy` for a
chatbot, a one-shot "generate X", or RAG over documents; a `manage_triggers` hook
for a fixed "when X happens, do Y" automation. An agent earns its cost only when
the model must decide, act, observe, and repeat.

<!-- extended -->

## Build the tools first
> when: wiring an agent that actually does something.

Create the `manage_function` handlers and the `manage_schema` tables the agent
needs BEFORE the agent endpoint, then list the function paths in `functions`. The
model can only call what the allowlist names; a tool it needs but cannot see is a
task it will fake or refuse. A listed path that is not a function yet is saved but
returns a `warning` (at run time the model only gets an "is not available" tool
error), so create each function first and spell the path exactly. On `update`,
`functions: "none"` and `cors_origins: "none"` clear them (an empty value means
"leave unchanged").

## Consuming from a page
> when: calling the agent from page JS.

```js
import { apiFetch } from 'auth';   // requires_auth is ON by default; apiFetch attaches the token
const res = await apiFetch('/api/assistant/run', {
  method: 'POST',
  body: { messages: [{ role: 'user', content: 'How many open orders do I have?' }] },   // apiFetch JSON-encodes it
});
const data = await res.json();     // data.content is the answer; data.stop_reason / data.steps describe the run
```

`/run` reads the `Authorization` header only; a plain `fetch` is enough ONLY when the
endpoint was created with `requires_auth: false`.

## When a run does not converge
> when: stop_reason comes back step_cap.

`step_cap` means the model kept asking for tools without answering. Usually the
tool set is too broad, a function keeps erroring, or the system prompt never tells
it when to STOP and answer. Tighten the prompt or the allowlist before raising
`max_steps` - a higher cap on a confused loop just costs more.

## Sub-agents: delegating subtasks
> when: the task has self-contained chunks the agent should hand off to keep its own context clean.

With `max_subagents > 0`, the agent gets a built-in `spawn_subagent` tool. It calls
it with a `task` string (and optionally a `tools` subset - the EXACT tool names from
its own set, `fn_<path>` / `query_data` / `http_fetch`; a misspelled name is silently
dropped and the sub-agent runs with fewer tools); a fresh sub-agent runs
that task with its own context and the same tools, then returns its answer as the
tool result. Sub-agents share the run's token budget and spawn count, and nest up
to `max_depth`. Use delegation for parallelizable or context-heavy chunks (research
a topic, work through one record end to end); keep it off (`max_subagents: 0`, the
default) for a simple assistant, where it only adds cost.

## Streaming a run
> when: the run is long enough that a page should show progress, not just wait.

Call `/run` with `?stream=1` (or an `Accept: text/event-stream` header) and the
endpoint streams Server-Sent Events instead of one JSON body: a `tool_call` /
`tool_result` frame per step (each tagged with its sub-agent `depth`) and ONE `text`
frame carrying the final answer (intermediate assistant text is not streamed),
a `notice` frame for a non-fatal in-run condition (the step cap or token budget
was hit - surface its `message`, it explains a run that stops without answering),
then one terminal `done` frame carrying `{content, steps, stop_reason, subagents,
usage}`. Parse the event NAME and JSON data per frame and buffer across reads, the
same way the `manage_llm_proxy` `/chat` stream is consumed; a hard failure arrives
as an `error` frame.

**Consume it with `fetch` + a `ReadableStream` reader, NOT `EventSource`.** EventSource
is GET-only: it cannot send the `messages` body or the `Authorization` header, so a
GET to `/run` without `?id=` is answered 400. The working shape,
identical to the proxy `/chat`:

```js
const res = await apiFetch('/api/support/run?stream=1', {   // apiFetch attaches the token
  method: 'POST',
  body: { messages: [{ role: 'user', content: q }] },
});
const reader = res.body.getReader(), dec = new TextDecoder();
let buf = '';
for (;;) {
  const { value, done } = await reader.read();
  if (done) break;
  buf += dec.decode(value, { stream: true });
  const frames = buf.split('\n\n'); buf = frames.pop();   // keep the partial frame
  for (const f of frames) {
    let name = 'message', data = '';
    for (const line of f.split('\n')) {
      if (line.startsWith('event: ')) name = line.slice(7).trim();
      else if (line.startsWith('data: ')) data += line.slice(6);
    }
    let p; try { p = JSON.parse(data); } catch { continue; }
    if (name === 'tool_call') showStep(p);
    else if (name === 'text') append(p.text);
    else if (name === 'notice') showNotice(p.message);
    else if (name === 'done') finish(p);
    else if (name === 'error') showError(p.error);
  }
}
```

## Async runs: jobs too long to wait on
> when: a run may take longer than an HTTP request should stay open.

`POST /run?async=1` starts the run in the background and returns `202` with
`{run_id, status:"running", status_url}` immediately, so the run survives the
client disconnecting. Poll `GET /run?id=<run_id>`: while it runs you get
`{status:"running"}`, and on completion `{status:"done", content, steps,
stop_reason, subagents, usage}` (or `{status:"failed", error}`). Use it for a
research agent or a long multi-step job; a quick assistant can just take the sync
JSON reply or the `?stream=1` stream.
