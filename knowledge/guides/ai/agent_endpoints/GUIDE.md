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

# Hosted agent endpoints (`manage_agent`)

Mints `POST /api/<path>/run` for multi-step tool-calling loops (use `manage_llm_proxy` for single-shot chat/RAG, `manage_triggers` for fixed event hooks).

**Tool allowlist (create functions/tables BEFORE the agent):**
- `functions` — CSV of `manage_function` paths, exposed as `fn_<path>` (non-alphanumeric chars become `_`, e.g. `lookup/order` → `fn_lookup_order`). Set each function's `description` and name `fn_*` tools in `system_prompt`. On `update`, `"none"` clears `functions`/`cors_origins` (empty leaves unchanged).
- `allow_data_query` — exposes read-only `query_data` (`SELECT` only; owner/tenant-scoped tables are withheld from non-admins/non-key callers — expose per-user data via `fn_<path>` filtering on `req.user.id`). Describe columns in `system_prompt`.
- `allow_http_fetch` — exposes SSRF-guarded `http_fetch`.
- `max_subagents` / `max_depth` — enables `spawn_subagent(task, tools?)` sharing the run's token budget.

**Request & response:**
- Request: `{ messages: [{ role: 'user'|'assistant', content }] }` (last message `user`; never send `system` — configured on the endpoint).
- Response: `{ content, steps, stop_reason, subagents, model, usage }` (`stop_reason`: `done`, `step_cap`, or `budget` — post-budget requests return `429`).
- Call `/api/<path>/run` directly from the browser (`apiFetch` when `requires_auth=true`, which is ON by default) — never wrap it in `manage_function`. For `?stream=1` SSE, use `fetch` + `ReadableStream` (not `EventSource`, which is GET-only and lacks `Authorization`).

**Bound cost:** always set `daily_token_budget` and `max_steps`, and keep `requires_auth: true` when the agent can write or spend tokens.

<!-- extended -->

## Streaming and async runs
> when: streaming progress (`?stream=1`) or running long background jobs (`?async=1`).

- **`?stream=1` (or `Accept: text/event-stream`):** emits SSE events `tool_call` / `tool_result` (tagged with sub-agent `depth`), `text` (final answer), `notice` (cap/budget `message`), `done` (`{content, steps, stop_reason, subagents, usage}`), or `error`. Parse `event:` and `data:` lines via `apiFetch` + `res.body.getReader()`.
- **`?async=1`:** returns `202` `{ run_id, status: "running", status_url }`; poll `GET /run?id=<run_id>` until `status: "done"` (`content`, `steps`, `stop_reason`, `subagents`, `usage`) or `"failed"`.
- **`step_cap`:** tighten `system_prompt` (tell the agent when to stop) or narrow `functions` before raising `max_steps`.
