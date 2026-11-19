---
slug: realtime_signaling
role: capability
title: Realtime signaling
summary: WebSocket realtime - rooms, presence, matchmaking, capacity, and authoritative server-reducer games (turn-based, hidden info, anti-cheat).
use_when: TWO OR MORE users share live state - multiplayer, presence, live collaboration, a lobby, or a chat room. The realtime model is a locked architecture decision, so include this whenever the project is genuinely multi-user.
not_when: A single-player game, a solo or offline tool, or a plain CRUD app. One user watching their own data change is not realtime.
evidence_gated: true
category: realtime
position: 21
tools: [manage_relay, manage_tick, manage_authoritative, manage_matchmaking, manage_realtime]
decisions: [realtime model/relay, realtime model/tick, realtime model/authoritative, realtime model/matchmaking]
---

# Realtime signaling (`manage_relay` / `manage_tick` / `manage_authoritative` / `manage_matchmaking`)

The WebSocket endpoint is a transparent JSON relay plus platform system frames; never invent your own join/leave protocol.

## Decision: realtime model (record in the plan's `decisions`)

Pick ONE model and record it as a plan decision (`topic: "realtime model"`):

- **Continuous shared state** (cursors, positions, shared canvas; plus optional discrete chat/events) -> `choice: "tick"` (**`manage_tick`**, `tick_hz` 20/30/60): `check: {artifact:"websocket", id:"<endpoint_path>", field:"tick_hz", op:"gt", value:0}`. Send continuous state via `net.send`, discrete events via `net.event`.
- **Authoritative** (contested, cheat-sensitive, hidden-info, auctions & bidding, ranked outcomes, durable economy like bankroll/chips/score) -> `choice: "authoritative"` (**`manage_authoritative`**, create then `set_reducer`): `check: {artifact:"websocket", id:"<endpoint_path>", field:"authoritative", op:"eq", value:1}`. Never use a client `create_api` POST to persist authoritative balances/scores.
- **Relay (default)** (discrete messages only: chat, notifications, online list) -> `choice: "relay"` (**`manage_relay`**): `check: {artifact:"websocket", id:"<endpoint_path>", field:"model", op:"eq", value:"relay"}`.
- **Matchmaking** (auto-pair opponents) -> `choice: "matchmaking"` (**`manage_matchmaking`**): `check: {artifact:"websocket", id:"<endpoint_path>", field:"matchmaking", op:"eq", value:1}`.
- **WebRTC calls** (P2P audio/video) -> `choice: "webrtc"` (**`manage_signaling`**, see `webrtc_signaling`): `check: {artifact:"websocket", id:"<endpoint_path>", field:"model", op:"eq", value:"signaling"}`.

**`tick` and `relay` run none of YOUR server-side code** (only `authoritative` runs a reducer): do not write a reducer or server validation into a `tick`/`relay` task — there is no server code there to run it. Conversely, **`authoritative` reducers are sandboxed with no imports**: server-authoritative physics must be pure-JS math on `@tick`, while real-time rigid-body physics (`cannon-es`, `Matter.js`, Phaser Arcade) runs client-side over `manage_tick` (`net.send` + `net.event`).

> **WebRTC is its OWN tool.** `manage_signaling` forwards SDP/ICE immediately and forces `tick_hz=0`; never use it for continuous state (`manage_tick`).

## System frames the server emits (clients receive, never send)

{{contract:realtime-frames}}

## The `ofnet` client helper (`js/ofnet.js`)

Platform-managed ES module — never create/edit/delete it. Import: `import { connect, roomCode } from 'ofnet'` (`clientId()` is this tab's reconnect secret, never your public id — use `net.myId`).

- **Your own relayed frames never echo back** — update local UI immediately after sending. Key player maps/ownership on server-stamped `_sender`/`_by`, never body fields.
- **Auth:** on `requires_auth`, `connect()` attaches `auth_token` as `?token=` automatically (raw `new WebSocket()` cannot authenticate); gate the page with `requireAuth`. Leave `requires_auth` and `tenant_column` off casual public multiplayer games.
- **Dispatch:** use `net.on(type, fn)` for all app frames AND `welcome`/`join`/`leave`/`error` (there is no `onWelcome`/`onJoin`/`onLeave`); `onPeers(fn)` for roster; `onMatched(fn)` for matchmaking; `onState(fn)` for authoritative; `onReconnect(fn)` for UI notices (reconnects and `welcome`/`onPeers`/`onState` replay are automatic).
- **Errors & limits:** subscribe to `net.on('error', ...)` (`{_type:'error', _message}`). Inbound rate cap is 120/sec; send continuous state at tick rate, not per rAF. Raw binary uses `net.ws` (rejected when `e2ee=true` — use `e2ee`'s `encryptBlob()`).
- **Split-page rule:** register `net.on(...)` inside an `export`ed init function called by the entry after `setNet(myNet)`, never at a leaf's top level (`client_js_split`).
- **Verify with two peers:** `manage_testing action=test_websocket endpoint_path=<p> two_peer=true` (add `app_action` for authoritative endpoints).

```js
const net = connect("game", { room: "world", meta: { name } });
net.onPeers((peers) => renderRoster(peers));
net.on('snapshot', (m) => { for (const [id, st] of Object.entries(m.states)) if (id !== net.myId) applyRemote(id, st); });
net.send({ type: 'move', x, y });   // continuous (coalesced on tick)
net.event({ type: 'chat', text });  // discrete (delivered whole)
net.onMatched((m) => startGame(m.role));
net.onState((s) => render(s));
```

<!-- extended -->

## Persisting messages (`write_to_table`)
> when: the endpoint must keep a history (late-joiners replaying a canvas/chat, or message logs).

Set `write_to_table` on the endpoint: it persists discrete frames (`net.event`, `relay`, `net.to()`), ignoring coalesced `net.send` frames. Frames missing a `NOT NULL` column are skipped, so declare required columns `NOT NULL` to filter junk. Never point `write_to_table` at a table written by REST or a reducer. Use `ephemeral: true` or `message_ttl_seconds` for per-session cleanup. On `e2ee=true`, only the `{iv,ct}` envelope persists.

## Room switching and `room_gated`
> when: a client changes room on a live socket, or uses `room_column` / `room_gated`.

Use `net.switchRoom("match-7", "<room_column>")` (keeps `net.room` in sync). The field name must match `room_column` (and the DB column if persisted). `room_gated` disables live switching and requires `?join=` (`welcome._joinToken` from the first peer -> `connect(path, { room, join: token })`).

## Identity, profiles, and presence
> when: wiring presence, names/avatars, or reconnect behaviour.

`welcome._clientId` (`net.myId`) is stable across tab refreshes; never send `clientId()` or pass a guessable `opts.cid`. Pass profiles at connect (`connect(path, { room, meta: { name } })`), read via `net.onPeers` / `net.peerMeta(id)` (max 2048 chars; room names max 128 chars). Derive online counts from `net.onPeers` / `_clients` only.

## Tick mode (spawn vs move + interpolation)
> when: continuous shared state (`tick_hz` 20/30/60).

- **Spawn/despawn ONLY in `net.onPeers`**, never from `snapshot.states` keys.
- **Move existing remote entities in `snapshot`**, skipping `id === net.myId` (move local entity directly from input).
- **Interpolate remote positions** toward the latest snapshot target in rAF (`lerp(target, 1 - Math.pow(0.001, dt))`) instead of snapping.
- Share one `packState`/`unpackState` contract in the store module.

## Writing the reducer (authoritative endpoints)
> when: the plan locked `realtime model = authoritative` and you are about to write or fix a reducer.

{{contract:room-reducer}}

## Authoritative games (wiring, seeding, settlement)
> when: the server must own state (turn-based, hidden-info, scored, or cheat-sensitive games).

Create via `manage_authoritative create`, then `set_reducer endpoint_path=<p> code=<js>` (to update later, `get` first, edit, and re-send `seed_*`/`shared_*` args on `set_reducer`):

```js
function reduce(state, event, ctx) {
  if (event.type === 'play' && event._by !== ctx.players[state.turn % ctx.players.length].id) ctx.reject('not your turn');
  if (event.type === '@timer' && event.name === 'settle') {
    for (const p of ctx.players) ctx.update('players', { bankroll: { '$increment': payout(state, p) } }, { id: p.uid });
  }
  return state;
}
```

- **Key `state.players` on `player.id`**, never `player.uid` (`uid` is `undefined` without `requires_auth`). Use `player.uid` only for durable DB writes. Make `@join` idempotent and mark `connected: false` on `@leave`.
- **Settle on round boundaries** (`@timer` / action), never in `@tick` (writes in `@tick` are dropped). Use `ctx.upsert` only on `UNIQUE`/`PRIMARY KEY` columns.
- **Seeding & global state:** pass `seed_table`, `seed_columns`, `seed_key_column` on `set_reducer` to populate `event.player.seed` at `@join`; pass `shared_table`/`shared_columns` for read-only `event.shared`.
- **Simulations:** add `tick_hz` to deliver `{type:'@tick', dt}` to `reduce` (small player counts only).

## Matchmaking
> when: "find me an opponent" / auto-pairing players into a shared room.

Use `manage_matchmaking` (`group_size`, optional `match_by: "rating"|"region"|"rating+region"` with `rating_band`, `band_widen_per_sec`, `max_wait_secs` and client `meta`). When matched, `ofnet` switches rooms and fires `net.onMatched((m) => ...)` (`{_type:"matched", _room, room_field, role, index, players}`).

**Matchmaking + authoritative reducer require TWO separate endpoints:** `set_reducer` is refused on a matchmaking endpoint. Pair on the lobby endpoint, then connect to an `ephemeral: true`, `max_clients: group_size` authoritative endpoint using `m._room`.

## Custom lobby, capacity, and cleanup
> when: hand-rolled lobbies, room caps, friend codes, or session cleanup.

- **Custom lobby:** derive one shared room id (`"match-" + [myClientId, peerId].sort().join("-")`) and break simultaneous challenges with `myClientId > peerId`; send handshake/finish frames with `net.event`/`net.to`.
- **Capacity & friend codes:** `max_clients=N` enforces caps server-side (`welcome._role: "spectator"` / `net.spectator`). Generate shareable codes with `roomCode()`, check live rooms via `GET /api/<path>/rooms`, and ensure create/join/invite links all target the same route the game JS attaches to.
- **Cleanup:** set `ephemeral: true` on the endpoint instead of `beforeunload`/`sendBeacon`.
- **Audio/video calls:** recall `webrtc_signaling` for `getUserMedia` + `RTCPeerConnection`.
