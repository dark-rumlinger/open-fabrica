---
slug: server_push
role: capability
title: Server push (SSE)
summary: One-way server->client live updates (feeds, tickers, dashboards) over Server-Sent Events - the EventSource stream shape, the row-change events, and when SSE beats a WebSocket.
use_when: The server pushes one-way live updates to viewers - a feed, ticker, dashboard, or notification stream that changes without a refresh.
not_when: A page that loads its data once, or a project where users send live state to each other (that is realtime_signaling).
evidence_gated: true
category: endpoints
position: 27
requires: [api_response_contract]
tools: [manage_sse]
---

# Server push over SSE (any project with create_stream)

`manage_sse` (a.k.a. `create_stream`) opens a one-way channel: the server pushes a message to the browser every time a row changes, and the client reads them with the built-in `EventSource`. No socket, no polling loop, no server code.

{{contract:endpoint-routes}}

## Consume with `EventSource` and listen by event name

The stream emits an SSE **event per row change**, named from the `trigger-events` catalogue - `data.insert`, `data.update`, `data.delete`. Each event's body is the changed row's visible columns **plus platform-added `table` (physical table), `endpoint` (publishing route), and `id`**. Register a listener per event name:

```js
const source = new EventSource('/api/prices/stream');
source.addEventListener('data.insert', (e) => {
  const row = JSON.parse(e.data);   // the row as stored, + table/endpoint/id
  prependTicker(row);
});
source.addEventListener('data.update', (e) => applyUpdate(JSON.parse(e.data)));
source.onerror = () => {/* usually leave empty - see below */};
```

Key your list on `id` and MERGE a `data.update` over what you hold, not swap the object.

The events are **named**, so a bare `source.onmessage` never fires - `addEventListener` the specific `data.*` names (`event_types` at create time filters which are sent).

**`EventSource` auto-reconnects AND the server replays what you missed** - write no reconnect logic and no refetch-on-reconnect for a short blip (that double-applies rows). Keep handlers idempotent (upsert by `id`) and a reconnect is invisible. The replay is best-effort (a ~1024-event in-memory window): after a long disconnect refetch once.

**Scope with the PHYSICAL TABLE NAME** (as passed to `create_table`), not the endpoint route: an endpoint at `orders/recent` over table `orders` wants `table_name: "orders"`.

## SSE or a WebSocket?

Pick SSE when the client only **reads** updates it did not ask for - live feeds, price/score tickers, a notification bell, a dashboard tile, a build-log tail. The lighter choice: one HTTP connection, auto-reconnect, row-change events for free, no relay.

Reach for `create_websocket` (see `realtime_signaling`) the moment the client must also **send** - chat, multiplayer, collaborative editing, presence. SSE has no client->server channel, so request/response and two-way messaging cannot be built on it.

<!-- extended -->

## Auth on a stream
> when: the live data is user-specific or sensitive.

`EventSource` cannot set an `Authorization` header (the same browser limitation a raw WebSocket has), so a `requires_auth` stream carries its token in the query string - the endpoint accepts `?token=`, exactly as a WebSocket does:

```js
const source = new EventSource('/api/prices/stream?token=' + encodeURIComponent(localStorage.getItem('auth_token')));
```

Gate the *page* with `requireAuth` (imported from `'auth'`) so the token is in `localStorage` before the stream opens; without a valid token the endpoint answers 401 and the stream never starts. Do not hand-roll a header onto `EventSource`, and do not leave a per-user stream public because gating "felt awkward".

**Row scoping is inherited, not configured.** A stream carries whole row-change events, so the server filters each one by the OWNING endpoint's access rules (`requires_auth`, `required_role`, `owner_column`, `tenant_column`): a subscriber receives a row only if it could have read that row over REST. There is nothing to set on the stream - scope the `create_api` endpoint and the stream follows. Two consequences worth planning around: a subscriber with no token receives nothing from an endpoint that requires auth (a `public_read` endpoint is the exception, it streams to anyone), and a `data.delete` event carries only the row id, so it cannot be attributed to an owner or tenant and is NOT delivered on an owner- or tenant-scoped stream (drive deletions from a status column you update instead). It IS delivered for `authored` and `public_read` endpoints, to admins, and to project-key subscribers. Keep columns nobody should see out of the streamed table regardless - scoping decides WHOSE rows, never which fields.

## What flows through the stream
> when: deciding which changes a client should see live.

The stream mirrors row changes made through the table's own `create_api` CRUD endpoint, so it is only as live as those writes - a value that changes via a `create_api` POST/PUT/DELETE shows up; one computed only in the browser does not. Writes that bypass the CRUD endpoint emit NO event: a `manage_function` `ctx.insert/update/upsert`, an upload auto-insert, a trigger's `insert_data`/`update_data`, a room reducer, and a CSV import. If those paths own the data, have the page fetch after they run, or route the change through the CRUD endpoint. If you need a derived or joined shape pushed live, stream the table whose writes represent the change, and compute the projection on the client from the pushed row.
