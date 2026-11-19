---
slug: server_functions
role: capability
title: Custom server functions
summary: A sandboxed server-side JS handler at /api/<path> for request logic CRUD, views, and triggers can't express - computed responses, external API calls with a server-kept key, multi-step validate-then-write.
category: endpoints
position: 28
tools: [manage_function]
---

# Custom server functions (`manage_function`)

Use `manage_function` only when declarative tools (`manage_api` CRUD/views, `manage_triggers`, `manage_schema` `generate`) cannot express the logic: computed responses, outbound API calls with server-kept secrets, or multi-step validate-then-write.

- **Paths & methods:** shares `/api/<path>` namespace with `manage_api` (dispatched by HTTP method; duplicate path+method is rejected). Defaults to `POST` unless `methods` is a comma-separated string (`"GET,POST"`). Literal paths only (no `:param`; use `req.query`/`req.body` or `manage_api` `lookup_column`).

## The handler contract

Synchronous function named `handler(req, ctx)` (no `async`/`await`, `require`/`import`, or `fs`):

```js
function handler(req, ctx) {
  // req = { method, path, query, headers, body, user: {id, role} | null }
  const rows = ctx.query('SELECT price FROM items WHERE id = ?', [req.body?.id]);
  if (!rows.length) return { status: 404, body: { error: 'not found' } };
  return { total: rows[0].price * (req.body?.qty || 1) }; // bare value -> 200 JSON; or {status, body, headers}
}
```

**`ctx` API (synchronous, bounded):**
- `ctx.query(sql, params)` — read-only single `SELECT` with `?` params (refuses `of_*` tables and PASSWORD/hash columns).
- `ctx.insert(table, row)` / `ctx.update(table, patch, where)` / `ctx.upsert(table, row, [keyCol])` — queued and applied in one transaction *after* `handler` returns (so a subsequent `ctx.query` won't see the new row). **`where` and `row`/`patch` are plain objects** (`{ id: 7 }`, never SQL strings); `patch` supports `{"$increment": n}` / `{"$decrement": n}`; `upsert` requires a `UNIQUE` index on `keyCol`.
- `ctx.fetch(url, { method, headers, body })` — SSRF-guarded outbound HTTP (`headers`/`body` must be strings).
- `ctx.secret(name)` — reads a secret on this endpoint's `secrets` allowlist.
- `ctx.llm(prompt, { system, max_tokens })` — completion on the project's configured LLM provider.
- `ctx.checkout({ amount, success_url, cancel_url, description, metadata })` — opens hosted checkout (`plan_id` for subscriptions; see `payments`).
- `ctx.now()` (ms) and `ctx.random()`.

**Auth:** with `requires_auth: true`, `req.user` is `{ id, role }`; scope per-user writes/queries to `req.user.id`. Handlers are validated at save time (`get` before `update`).

<!-- extended -->

## Errors and limits
> when: writing a handler that could fail or do heavy work.

- Uncaught throws become `500` (client sees no internal message); return `{ status: 4xx, body: {...} }` for controlled errors or wrap `ctx.*` calls in `try/catch`.
- Limits: 30s default wall-clock (max 120s via `timeout_ms`; timeouts return `504`, in-build probes `test_endpoint`/`test_url` wait ~60s), 256 KB request body (`Authorization`/`Cookie` stripped), `ctx.fetch` 8 calls / 256 KB / 15s, `ctx.llm` 4 calls (`max_tokens` ≤ 4096), `ctx.checkout` 3 calls, `cors_origins` is a comma-separated string.

## Call it from a page
> when: wiring the client to the endpoint.

```js
const res = await fetch('/api/quote', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' }, // use apiFetch (from 'auth') if requires_auth=true
  body: JSON.stringify({ id: 7, qty: 3 }),
});
const data = await res.json();
```
