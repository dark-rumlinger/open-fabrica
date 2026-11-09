---
slug: event_actions
role: capability
title: Event-driven triggers
summary: "When X happens, do Y" server-side - the event catalogue, payload vars, action types, and filters (all triggers fire after the write commits; no before-hooks).
use_when: A data change or event should AUTOMATICALLY run a server-side action - a welcome email on signup, an order confirmation, a low-stock alert, an audit row, an embed-on-insert. Fires even with no page open.
not_when: Logic that must run inside the request that writes the data (validation, derived columns) belongs in that function, not an after-commit trigger; and purely client-side UI reactions are not triggers.
category: automation
position: 40
tools: [manage_triggers]
---

# Event-driven actions (any "when X happens, do Y" feature)

A trigger runs a server-side action when an event fires - a signup welcome email, an order-confirmation, a low-stock alert, an audit row, an embed-on-insert. It always beats client-side glue (it can't be skipped by a closed tab and runs with no page open). Define one with `manage_triggers` (`create` / `list` / `update` / `delete` / `test`). Do NOT hand-roll this as `fetch` calls in page JS or a polling loop.

The two halves of a trigger: WHICH event it listens to (`event_type`, optionally narrowed by `event_filter`) and WHAT it does (`trigger_type` + `trigger_config`).

## The event catalogue (what you can listen to)

{{contract:trigger-events}}

Reach a payload value in the config with `{{field}}` (e.g. `{{email}}`, `{{id}}`, `{{table}}`); an unknown placeholder is left literal.

## The action types (what a trigger does)

`trigger_type` is a closed set; `trigger_config` shape depends on it:

- **`send_email`** - `{to, subject, body_html | template_name, template_vars}` (dispatches via `manage_email`).
- **`send_push`** `{endpoint_path, title, body, url?, user_id?}` / **`send_notification`** - message a user (`send_notification`: `{user_id, title, message, type, action_url?}`, in-app).
- **`insert_data`** - `{table, data:{col:"{{...}}"}}`; **`update_data`** - `{table, set:{col:val}, where:{col:"{{id}}"}}` (set supports `{"$increment":1}` / `{"$decrement":1}`).
- **`http_request`** - `{method, url, headers, body}` to an external URL; **`trigger_webhook`** - `{webhook_name, payload}` to a registered outgoing webhook (`manage_webhooks`).
- **`ws_broadcast`** - `{endpoint_path, room, message, tenant_id?}` pushes to a `create_websocket` room (live badges). The frame arrives byte-for-byte, so **`message` MUST carry a `type`** or no `net.on(...)` handler fires. `endpoint_path` is the BARE create-time path (no `/api/`, no `/ws`), `room` must equal the clients' `?room=`, tenanted endpoints need `tenant_id`, and a server push has no `_sender`.
- **`run_sql`** - `{sql, params}` for a computed aggregate. SECURITY: never put `{{...}}` in `sql` - it is refused; pass payload values in `params` as `?` placeholders (`{"sql":"UPDATE stats SET orders = orders + 1 WHERE id = ?","params":["{{id}}"]}`). One SELECT/INSERT/UPDATE only.
- **`enqueue_job`** - `{type, payload}` defers work to the job queue; `type` is `send_email`, `http_request`, `run_sql` or `llm_prompt` only.
- **`run_function`** - `{endpoint_path}` invokes a `manage_function` handler with the whole event payload as `req.body` - the escape hatch for computed logic the other types can't express (it persists its own result; the return value is discarded).
- **`llm_call`** - sends the expanded `prompt` to the LLM, NOT the event payload: pull row fields in with `{{column}}` (see the `llm_endpoints` guide).
- **`ai_embed`** - keep a row embedded so `manage_vector_search action=semantic_search` finds it. Needs the table's vector index first (`manage_vector_search action=create_vector_index`); then config `{}` on `data.insert`/`data.update` embeds that row's indexed columns. `ref_table`/`ref_id`/`text` override the row or text.

## Narrowing

- **`event_filter`** scopes the listener: `data.insert` fires for EVERY table, so a per-table trigger needs `event_filter: {"table":"orders"}`. ALL filter keys must equal the payload, exactly.

## Don't reach for a trigger when a simpler tool fits

A trigger is for reacting to an EVENT. Periodic/clock-driven work (a nightly digest, hourly cleanup) is a schedule (`manage_schedules`), not a trigger. A one-off side effect inside a page flow that the user waits on is a normal API call. Use a trigger when the cause is one of the catalogue events above.

<!-- extended -->

## Action-type details
> when: a trigger config misbehaves - job types, function context, HTTP defaults, retries.

- `enqueue_job` also takes `scheduled_at` and `max_attempts`. Valid job types are `send_email`, `http_request`, `run_sql` (read-only SELECT/VALUES) and `llm_prompt`; any other type fails with "unknown job type" after its retries.
- `run_function` runs the handler with `req.user = null` and `req.method = "event"`; it must persist its own result with `ctx.insert/update/upsert`.
- `llm_call` sends only the expanded `prompt`; `max_tokens` defaults to 512, so set it for a long summary.
- `http_request` defaults `method` to POST and refuses private and metadata addresses. `trigger_webhook` posts the raw event payload (or its `payload` config) as-is, while a `manage_webhooks subscribe` wraps it in an `{event, project_id, payload, timestamp}` envelope; both are signed and retried up to 3 times on a network error, 5xx or 429.

## Common recipes
> when: wiring a concrete trigger and you want the exact config shape.

- **Welcome email on signup:** `event_type:"auth.register"`, `send_email`, `{to:"{{email}}", subject:"Welcome", template_name:"welcome"}`.
- **Grant access / fulfil on payment:** `event_type:"payment.completed"`, `update_data` to flip the order/entitlement row keyed by `{{external_id}}` (see the `payments` guide - fulfilment MUST be a trigger, never the success redirect).
- **Audit log:** `event_type:"data.delete"` + `event_filter:{"table":"orders"}`, `insert_data` into an `audit` table.
- **Denormalised counter:** `data.insert` + `run_sql` `{"sql":"UPDATE authors SET post_count = post_count + 1 WHERE id = ?","params":["{{author_id}}"]}`.
- **Live badge:** `data.insert` on `orders` + `ws_broadcast` `{"endpoint_path":"dashboard","room":"admins","message":{"type":"new_order","id":"{{id}}","total":"{{total}}"}}`, received with `net.on('new_order', renderBadge)`. The `type` is what makes the handler fire.

