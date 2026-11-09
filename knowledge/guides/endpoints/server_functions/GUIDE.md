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

Reach for a function endpoint when a request needs **computed server logic** the declarative tools can't express: a custom response shape, an external API call with the key kept server-side and the result reshaped, multi-step validate-then-write, or a derived/aggregated payload. `manage_function` mounts one model-authored handler at `/api/<path>`.

**Choose the right tool first, a function is the escape hatch, not the default:**
- Plain table reads/writes (`POST/GET/PUT/DELETE` over one resource) -> `manage_api` (`create_api`). Joined/aggregated read-only rows -> a view endpoint. Don't hand-write CRUD in a function.
- "When X happens, do Y" side-effects fired off a data/auth/payment event (a welcome email, a stock alert) -> `manage_triggers`. Don't poll or reimplement that in a function.
- A **server-generated column value** at insert (a share code/slug, an invite token, a uuid, an `expires_at`) -> a `manage_schema` column with `generate` (`code`/`token`/`uuid`/`expires_in_days:N`). The DB fills it on every insert, including an upload/CRUD auto-insert - don't hand-write a function just to mint a slug or stamp an expiry.
- The request needs logic none of those express (compute, an outbound call, conditional multi-step work, a bespoke response) -> a function.

**Paths are shared across endpoint kinds; dispatch is by HTTP method.** A function and a `manage_api` CRUD endpoint can both sit at the same `<path>` - the router sends each request to whichever kind declares its method. So a POST-only "create" function and a GET lookup CRUD endpoint can deliberately share `/api/<path>`, but two kinds may **not** claim the same path *and* method: the second registration is rejected up front (pick a different method or a distinct path). A method no kind at the path declares returns 405. A function answers **POST only** unless `methods` is a comma-separated STRING (`"GET,POST"`, not an array). A function also has **no path parameters**: it matches its exact literal path only (`:slug` is rejected), so a dynamic value must arrive via `req.query` or `req.body`. To fetch one row by a URL segment (`/api/files/<slug>`), use a `manage_api` endpoint with `lookup_column` - not a function. When in doubt, give the function its own suffix (e.g. `files/create`).

## The handler contract

The `code` is one **synchronous** function. It MUST be named `handler`, take `(req, ctx)`, and RETURN the response. No `async`/`await` (ctx calls are synchronous and there is no event loop); no `require`/`import`; no `fs`/network beyond `ctx.fetch`.

```js
function handler(req, ctx) {
  // req = { method, path, query, headers, body (parsed JSON or string), user: {id, role} | null }
  const { id, qty } = req.body || {};
  const rows = ctx.query('SELECT price FROM items WHERE id = ?', [id]);   // read-only SELECT
  if (!rows.length) return { status: 404, body: { error: 'not found' } };
  return { total: rows[0].price * (qty || 1) };   // bare value -> 200 JSON
}
```

**Return shape.** Either an envelope `{ status, body, headers }` or a bare value (becomes `200` JSON). A **string** `body` is sent verbatim (return `{ headers: { 'Content-Type': 'text/html' }, body: '<h1>hi</h1>' }` for HTML); any other value is JSON-encoded.

**`ctx` - the only I/O, all injected and bounded:**
- `ctx.query(sql, params)` - a **read-only** single `SELECT` against the project DB (parameterize with `?`; only `SELECT` runs - stacked statements are refused, and system `of_*` tables and PASSWORD/hash columns are refused, with hash columns stripped from `SELECT *`). Returns an array of row objects.
- `ctx.insert(table, row)` / `ctx.update(table, patch, where)` / `ctx.upsert(table, row, [keyCol])` - **declare** a durable write (`upsert`'s key columns are REQUIRED and UNIQUE-indexed; `update` needs a non-empty `where` and `patch`, else the request ends in a generic 500). Writes are applied in ONE transaction AFTER the handler returns successfully (never mid-run). Because of that, a later `ctx.query` in the SAME invocation CANNOT see a row you just inserted - the write hasn't run yet; build the response from values you already hold (the `code`/`id`/`row` you generated), not a read-back. `patch` supports `{"col": {"$increment": n}}` / `{"$decrement": n}`. Writes to `of_*` tables or PASSWORD/ENCRYPTED columns are refused.
  **`where` is an OBJECT of equality matches, never a SQL string.** `ctx.query` above takes SQL, these do not: every argument to a write is a plain JS object, and the platform builds the statement. Pass `{ id: 7 }` or `{ user_id: uid, post_id: pid }` (keys AND together); a string like `'id = ' + id` throws a TypeError the moment the call runs, and because the response for a thrown handler is a generic `500` you will not see the type in the body. The same holds for `row`/`patch` (objects) and `upsert`'s third argument (an array of column names).
  ```js
  ctx.update('posts', { score: { '$increment': 1 } }, { id: postId });   // yes
  ctx.update('posts', { score: { '$increment': 1 } }, 'id = ' + postId); // throws
  ```
  Prefer `$increment`/`$decrement` over reading a value and writing back the sum: the operator updates in one statement, so two requests landing together both count. A read-then-write loses one of them.
- `ctx.fetch(url, { method, headers, body })` - one **SSRF-guarded** outbound request (private/link-local/metadata IPs are blocked). Returns `{ status, headers, body }` (`body` is text). `body` and `headers` values must be strings (`JSON.stringify` an object).
- `ctx.secret(name)` - the decrypted value of a stored secret, but ONLY names on this endpoint's `secrets` allowlist. This is how an external API key stays server-side: grant it, read it here, put it in a `ctx.fetch` header, never send it to the browser.
- `ctx.llm(prompt, { system, max_tokens })` - one completion from the project's **already-configured** LLM provider (no separate key needed). Returns the text. Reach for it to summarize/classify/extract/draft. Bounded per invocation; each is a real, billable completion.
- `ctx.checkout({ amount, success_url, cancel_url, description, metadata })` - opens a hosted payment checkout on the project's configured provider and returns `{ checkout_url, payment_id }` (`plan_id` instead of `amount` for a subscription). The `payments` guide owns the flow around it.
- `ctx.now()` - current time in ms. `ctx.random()` - a float in [0,1).

**Auth.** With `requires_auth: true`, an unauthenticated call is refused and `req.user` is `{ id, role }`; otherwise `req.user` is `null`. Scope any per-user query/write on `req.user.id`, never on a client-supplied id.

The handler is validated at save time with the same compiler the runtime uses, so a handler that would not load is rejected with the reason when you save it (read the current source with `action=get` before fixing a bug, then `update`).

<!-- extended -->

## External API call with a server-kept key
> when: the project integrates a third-party API (weather, geocoding, Slack, an LLM you proxy yourself).

Store the key once (`manage_chat ask`, `question_type: "secret"`), grant it on the function's `secrets` allowlist, and never let it reach the browser:

```js
function handler(req, ctx) {
  const key = ctx.secret('WEATHER_KEY');
  const res = ctx.fetch('https://api.example.com/v1/current?city=' + encodeURIComponent(req.body.city) + '&key=' + key);
  if (res.status !== 200) return { status: 502, body: { error: 'upstream failed' } };
  const data = JSON.parse(res.body);
  return { tempC: data.temp_c, summary: data.summary };   // reshape - the browser never sees the key or the raw payload
}
```

## Fetch external data and summarize it with the LLM
> when: a scheduled/triggered job (or endpoint) pulls data and needs the configured LLM to condense it.

`ctx.llm` uses the provider the owner already configured - no second key. A classic
scheduled function (a `run_function` native action on a cron/interval task) fetches,
summarizes, and stores in one pass:

```js
function handler(req, ctx) {
  const res = ctx.fetch('https://api.example.com/feed?user=xyz&max=10', {
    headers: { Authorization: 'Bearer ' + ctx.secret('FEED_TOKEN') }
  });
  const items = (JSON.parse(res.body).items || []).map(i => i.text).join('\n');
  const summary = ctx.llm('Summarize these posts in 3 bullets:\n' + items, { max_tokens: 200 });
  ctx.insert('digests', { account: 'xyz', summary, at: new Date().toISOString() });
  return { ok: true };
}
```
The default time budget covers a single fetch-plus-summarize; for several external
calls in one handler, raise `timeout_ms`.

## Errors and limits
> when: writing a handler that could fail or do heavy work.

- A **thrown** exception (or a `ctx` call that throws, a bad SELECT, a blocked fetch URL, an ungranted secret) becomes a `500`; the client never sees the internal message. To return a controlled error, `return { status: 4xx, body: {...} }` instead of throwing. You can `try { ctx.query(...) } catch (e) { ... }` to recover.
- Bounded per invocation: wall-clock (30s default, max 120s via `timeout_ms`; a timeout answers **504**, not 500), response size, declared writes, `ctx.query`/`ctx.fetch` call counts, and a 256 KB request body (`Authorization` and `Cookie` headers are stripped from `req.headers`; a repeated query key arrives as an array). Specific caps: `ctx.fetch` 8 calls, 256 KB response, 15 s each, string `body`/headers only; `ctx.llm` 4 calls with `max_tokens` clamped to 4096; `ctx.checkout` 3 calls; `cors_origins` is a comma-separated string like `methods`. A runaway loop is interrupted and fails that one request. The in-build probes (`test_endpoint`/`test_url`) wait only ~60s, so a synchronous handler is verifiable up to about that long; a larger `timeout_ms` still runs but cannot be proven green in-build, so keep synchronous work under that and move anything longer off the request path.
- Persist through **declared writes**, not a side channel. Don't try to open your own DB connection or call another endpoint to write, `ctx.insert/update/upsert` is the durable path, applied transactionally after you return.

## Call it from a page
> when: wiring the client to the endpoint.

```js
const res = await fetch('/api/quote', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },   // requires_auth? call it through apiFetch (from 'auth') instead - it adds the Bearer header (see the auth contract)
  body: JSON.stringify({ id: 7, qty: 3 }),
});
const data = await res.json();   // whatever the handler returned as the body
```
