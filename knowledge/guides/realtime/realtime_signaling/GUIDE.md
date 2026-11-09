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

# Realtime relay protocol (any realtime endpoint: manage_relay / manage_tick / manage_authoritative / manage_matchmaking)

The WebSocket endpoint is a transparent JSON relay PLUS three system frames the server emits on its own (on EVERY endpoint); build clients against this contract, never your own join/leave protocol. Every tool here creates a **websocket endpoint** (the `create_websocket` capability), so the `ofnet` helper and system frames apply to every model.

## Decision: realtime model (record in the plan's `decisions`)

Pick ONE model and record it as a plan decision (`topic: "realtime model"`):

- **Continuous shared state** (live cursors, positions, a shared canvas) -> `choice: "tick"`: a **`manage_tick`** task, `tick_hz` 20/30/60 (the server coalesces per-room snapshots). `check: {artifact:"websocket", id:"<endpoint_path>", field:"tick_hz", op:"gt", value:0}`. **Continuous PLUS discrete messages (movement plus chat/emotes - most multiplayer games) is still this case: ONE tick endpoint, ONE task - `net.send` for movement, `net.event` for chat.** A whiteboard too: cursors `net.send`, strokes/clear/undo `net.event` (a `net.send` stroke is coalesced away).
- **Authoritative** - contested / cheat-sensitive / hidden-info state the server must own (turn-based & hidden-hand games, auctions & bidding, seat/inventory booking under contention, live voting/quizzes, arbitrated collaboration). An action game belongs here too when its OUTCOME ranks players (score, kills, ladder, wagers): the server owns hit and score resolution even while it ticks positions. Casual combat for feel stays tick. -> `choice: "authoritative"`: a **`manage_authoritative`** task (create, then `set_reducer`). `check: {artifact:"websocket", id:"<endpoint_path>", field:"authoritative", op:"eq", value:1}`. A **durable economy** (bankroll, chips, score, win/loss history) is the reducer's to seed and settle (Authoritative games below); NEVER a client-side "POST my result / update my balance" task - a client writing its own money is cheatable ("the house has no control").
- **Relay** (default) - discrete messages ONLY, no continuous state anywhere in the app (chat room, online list, live notifications) -> `choice: "relay"` (**`manage_relay`**). `check: {artifact:"websocket", id:"<endpoint_path>", field:"model", op:"eq", value:"relay"}`. If anything streams positions or cursors, pick tick and carry the messages there.
- **Matchmaking** (auto-pair "find me an opponent") -> `choice: "matchmaking"`: a **`manage_matchmaking`** task (an authoritative matched game pairs here but plays on its own endpoint - Matchmaking below). `check: {artifact:"websocket", id:"<endpoint_path>", field:"matchmaking", op:"eq", value:1}`.
- **WebRTC calls** (peer-to-peer audio/video) -> `choice: "webrtc"`: a **`manage_signaling`** task; follow the `webrtc_signaling` guide. `check: {artifact:"websocket", id:"<endpoint_path>", field:"model", op:"eq", value:"signaling"}`.

**`tick` and `relay` run none of YOUR server-side code and validate none of your message content** - only `authoritative` runs a reducer (the platform still enforces capacity, rate, size, and persistence on every endpoint). When a requirement reads "the server assigns / owns / validates / expires" something, it is EITHER `authoritative` (choose it and write the reducer) OR, for cosmetic assignment (a random join colour), a roster, or presence, CLIENT-SIDE plus the platform's presence frames (`net.onPeers` and welcome/join/leave below) - never a reducer. Do NOT write a reducer, roster ownership, or server-side validation into a `tick`/`relay` task; there is no server code there to run it. (`authoritative` MAY also set `tick_hz` to simulate continuously; that stays `choice: "authoritative"`, not `tick`.)

> **WebRTC is its OWN tool.** `manage_signaling` forwards SDP/ICE immediately and forces `tick_hz=0`; never bolt it onto a relay/tick endpoint or use it for continuous state - no `snapshot` frames, nobody sees anyone move (that is `manage_tick`).

## System frames the server emits (clients receive, never send)

{{contract:realtime-frames}}

## Presence

Drive any "players online" count from ONE source (the `net.onPeers` roster length, or `_clients`), never counts each side derives differently.

## Rooms, and the frames you send

Your own relayed frames never echo back - update local UI right after sending. Key every player map, turn check, and ownership check on the server-stamped `_sender` / `_by` (contract above), never on a body field.

Pick the room with `?room=` at connect (`...?room=lobby`); omit for one global channel. Switching rooms on a live socket, and the `room_column` it needs: **Room switching** below.

## The realtime client helper (the `ofnet` module) - method names

`js/ofnet.js` is a platform-managed ES module; import by specifier: `import { connect, roomCode } from 'ofnet'` (its `clientId()` export is this tab's private reconnect secret, never your id - that is `net.myId`). **Every frame TYPE except `matched`/`state` arrives through `net.on(type, handler)`** - it dispatches on an app frame's `type` (the contract's discriminator) and a system frame's `_type` alike. There is **no `onWelcome`/`onJoin`/`onLeave`**: those are `on('welcome'|'join'|'leave', …)`; presence wants `onPeers`. `matched`/`state` fire ONLY `onMatched`/`onState`. The other `onX` methods are NOT per-frame-type sugar: `onOpen`/`onClose`/`onError` fire on the raw socket (NOT the `welcome` frame), `onMatched`/`onState` are endpoint modes, and `onMessage(fn)` is a raw catch-all that receives every server frame once, exactly as sent (including `welcome`/`join`/`leave`/`matched`/`state`/`error`), never the helper's own open/close/roster events:

> **Auth is automatic - never hand-roll it.** On `requires_auth`, `connect()` auto-attaches the logged-in user's token (the `auth_token` key the auth contract defines - `apiFetch`'s source) as `?token=` on the connect URL; you do NOT pass a token. A **raw `new WebSocket()` will NOT authenticate** - a browser socket cannot send an `Authorization` header, so `requires_auth` endpoints are reachable ONLY via that `?token=`. Use `connect()` (never a raw socket), and gate the *page* with `requireAuth` (from `'auth'`) so the token exists in `localStorage` before `connect()` runs - the page gate is no substitute for `requires_auth`.

**Casual multiplayer games are usually NOT `requires_auth`.** A public game needs no login: leave it off; the per-tab `?cid=` `connect()` sends identifies players. Rooms are shared across everyone - a room is tenant-isolated ONLY on an endpoint with a `tenant_column`, so do not add one to a game endpoint (tenant-scoped rooms are what makes "players can't see each other"). Two browsers on the SAME account share one client id (`u<userId>`) and kick each other off, so use distinct accounts or keep the endpoint anonymous.

```js
const net = connect("game", { room: "world", meta: { name } });
net.onPeers((peers) => renderRoster(peers));    // roster = WHO exists (spawn/despawn here); whole list on connect AND every change
net.on('snapshot',(m) => { for (const [id, st] of Object.entries(m.states)) if (id !== net.myId) applyRemote(id, st); }); // tick_hz>0: MOVE existing entities, never spawn here
net.on('move',    (m) => applyMove(m));         // your custom frame types dispatch the same way
net.send({ type: 'move', x, y });               // CONTINUOUS (coalesced on tick; no echo to you)
net.event({ type: 'chat', text });              // DISCRETE, arrives whole (relay AND tick)
net.onMatched((m) => startGame(m.role));        // matchmaking endpoints only
net.onState((s) => render(s));                  // authoritative endpoints only (server-pushed state)
net.peers();                                    // roster read once   roomCode() -> a shareable code
net.onReconnect(() => toast('reconnected'));    // a dropped socket re-dialed the same room (UI only; onPeers/onState re-deliver)
```

**Reconnects are automatic.** A socket that drops after connecting re-dials the same room (after a `switchRoom`, the new one) with the same identity, re-queuing what you sent meanwhile; the server replays `welcome`, so `onPeers`/`onState` re-deliver on their own - do NOT write your own reconnect loop. `net.reconnecting` is true while it re-dials; `connect(path, {reconnect:false})` opts out. A connection replaced by a newer one with the same id (close code 4001) is not re-dialed.

**Verify it with two real peers:** `manage_testing action=test_websocket endpoint_path=<p> two_peer=true` (add `app_action={"type":"<ACTION>",...}` on an authoritative endpoint to drive one action through the reducer) - a create result proves the relay speaks, not that two players see each other.

**Subscribe to `net.on('error', …)`.** Rejected reducer actions, spectator sends, oversize or rate-dropped frames and e2ee plaintext rejections arrive as `{_type:'error', _message}` and are only console-logged - nothing reaches the user unless you render it. Inbound frames are capped (`ws_inbound_rate`, 120/sec, tunable on `manage_relay` only) - send continuous state at the tick rate, not per animation frame.

Use `onMatched`/`onState` ONLY on those endpoint kinds. Need a raw binary frame? The socket is `net.ws` (there is no `net._ws`); `send`/`event`/`to`/`action` cover every normal case. NOT on `e2ee=true`: the relay REJECTS raw binary there - send encrypted blobs through the `e2ee` session's `encryptBlob()`, whose chunk shape is fixed by the [[e2ee]] guide's wire contract (NOT the `{iv,ct}` chat envelope).

**In a split page, register `net.on(...)` handlers from an `export`ed init fn the entry calls right after `setNet(myNet)` - never at a leaf's module top level.** A leaf's top level runs while `net` is still the `null` live-binding seed, so handlers attach to nothing - `onPeers` never fires, the roster stays empty, the lobby never advances (every probe still green). The null-seed rule: `client_js_split`.

<!-- extended -->

## Persisting messages
> when: the endpoint must keep a history - late joiners replaying a canvas or chat, or any message log.

**Late-join history rides the two lanes.** Set `write_to_table` on that one tick endpoint: it persists the `net.event` strokes and ignores the `net.send` cursors - the lanes decide, you filter nothing by hand. Persisting commits you to cleanup: a per-match / per-session room needs `ephemeral:true` (rows drop once the room empties) or `message_ttl_seconds`.

> **`write_to_table` persists the durable lane; the TABLE'S SHAPE filters frames.** It auto-inserts each frame delivered whole (every relay frame; tick's `net.event` frames; `net.to()` frames), mapping JSON fields to columns; coalesced `net.send` state never persists. A frame that cannot fill a NOT NULL column is skipped quietly - declare the durable shape's columns NOT NULL to keep junk out (all-NULLable = every frame lands). Never point it at a table another path writes (a REST upsert, a reducer's `ctx.upsert`): broadcast frames usually lack that path's key column - persist edits through ONE path. On `e2ee=true` only the `{iv,ct}` ciphertext envelope persists (the archive lane) - see the [[e2ee]] guide.

## Room switching
> when: a client changes room on a live socket (lobby into a match), or you are configuring `room_column`.

A `room_column` endpoint lets a client switch rooms live by sending that exact field: use the `ofnet` helper's **`net.switchRoom("match-7", "<room_column>")`** (it sends the field AND updates `net.room`), not a hand-rolled `ws.send(JSON.stringify({ <room_column>: "match-7" }))` - the raw send reaches the server but leaves `net.room` stale, so later reads of it lie. Either way the server moves you and replays a fresh welcome + join/leave. The switch frame is consumed as control plumbing (not relayed, not persisted) when the move succeeds and the room actually changes; a switch to the room you are already in is relayed like any frame. Peers learn of you from the join frame. The field name MUST equal the configured `room_column` AND, when the endpoint persists messages, the DB column storing the room (ephemeral prune is scoped on it; a mismatch silently breaks switching and/or cleanup). `room_gated` endpoints fix the room at connect - no live switching, and ENTRY itself needs a token: the first peer claims the room and gets `welcome._joinToken`, later peers pass it back as `connect(path, { room, join: token })` (the `?join=` the helper forwards). Wire that room-gated handshake per the [[e2ee]] guide (it is the same `?join=` flow, encryption or not).

## Identity and profiles
> when: wiring presence - names/avatars on the roster, or reconnect behaviour after a refresh.

`welcome._clientId` (= `net.myId`) is STABLE across reconnects (`connect()` sends a persisted private `?cid=` the server hashes into it; on `requires_auth` it is the authenticated user), so a refresh takes back its slot and the old `leave` is suppressed - no "opponent disconnected" ghost. Key presence by `_clientId`; a returning id is the same player. The `?cid=` is per TAB (sessionStorage), so two tabs are two players and a refresh keeps its slot. It (and `clientId()` from `'ofnet'`) is a reconnect SECRET, not your id - never send it in a frame, and never pass a guessable `opts.cid` (a user id, a name): whoever knows a cid can take over that slot. For account identity use `requires_auth`. `_clients` INCLUDES the arrival; on a capped room a `join`'s `_role` is `"player"`/`"spectator"` (the joiner gets no join frame of its own).

**Profiles (names/avatars): pass them at CONNECT, don't announce them.** `connect(path, {room, meta:{name}})`; profiles ride the `net.onPeers` roster (`net.peerMeta(id)` reads one). Never broadcast your own profile in an app frame after connecting - peers already in the room never receive it (the flaky hand-rolled "JOIN_LOBBY" scheme). Limits: `meta` over 2048 characters (or invalid JSON) is silently ignored, so keep avatars as short ids or URLs, never data URLs; a room name is at most 128 characters, and one visitor may hold at most 100 sockets per project.

## Tick mode
> when: continuous shared state (cursors, positions) the server coalesces into room snapshots at tick_hz 20/30/60.

A `tick_hz>0` endpoint (20/30/60 typical, up to 120) coalesces `net.send` frames (snapshot frame: contract above) - only the latest frame before each tick survives; reading your own id off `states` overwrites local state with a one-tick-stale copy (rubber-banding).

Spawning from `states` (or listing `Object.keys(m.states)`) is the classic "players can't see each other" bug. Skeleton:

```js
const others = new Map();                                    // id -> entity
net.onPeers((peers) => {                                     // ROSTER: who exists
  const live = new Set(peers.map((p) => p.id));
  for (const p of peers) if (p.id !== net.myId && !others.has(p.id)) others.set(p.id, spawn(p));
  for (const id of [...others.keys()]) if (!live.has(id)) { despawn(others.get(id)); others.delete(id); }
});
net.on('snapshot', (m) => {                                  // DELTA: move who already exists
  for (const [id, st] of Object.entries(m.states)) {
    if (id === net.myId) continue;                           // skip self (rubber-banding)
    const e = others.get(id); if (e) moveToward(e, st);      // UPDATE only - never spawn here
  }
});
```

**The frame you `net.send({...})` and the `st` you read off `states` are the SAME object**, so it is one shared payload shape (project_state's Module contracts section owns that rule). Give it an exported `packState(local)` / `unpackState(frame)` pair in the shared store module and route both sides through it.

**Remote state must INTERPOLATE, never snap.** Snapshots arrive at `tick_hz` (~33-50ms apart) but you render at ~60fps; writing values straight onto the entity teleports it `tick_hz` times a second. In `applyRemote`, only record a TARGET (`e.target.set(st.pos[0], st.pos[1], st.pos[2])`), then ease toward it in the rAF loop: `e.mesh.position.lerp(e.target, 1 - Math.pow(0.001, dt))` (frame-rate-independent; `dt` = seconds since last frame). Your OWN entity is the exception: move it directly from input with delta-time at full rAF, NEVER from the snapshot.

(`net.to(id, …)` and binary `net.ws.send(blob)` also arrive immediately on a tick endpoint; an ordinary room-wide message needs neither.)

## Writing the reducer (authoritative endpoints)
> when: the plan locked `realtime model = authoritative` and you are about to write or fix a reducer.

If you chose `authoritative`, the reducer's contract follows - read it before writing one; the sandbox deliberately throws on things that work everywhere else. (On an authoritative build this contract also rides your build seed up front.)

{{contract:room-reducer}}

Worked example, seeding, matchmaking handoff, hidden-info `view()`: **Authoritative games** below.

## Authoritative games
> when: the server must own state - turn-based, hidden-info, scored, or cheat-sensitive games (server-side reducer).

Create with `manage_authoritative create`, then `manage_authoritative set_reducer endpoint_path=<p> code=<js>`. The reducer's contract is in the `Writing the reducer` section (and in your build seed on an authoritative build); this is the worked example and wiring. (DB-centric flows can instead keep the truth in a TABLE and push with a `ws_broadcast` trigger on `data.insert/update/delete`; the reducer is cleaner for game logic.)

```js
function reduce(state, event, ctx) {
  if (event.type === '@init')  return { turn: 0, moves: [] };
  if (event.type === '@join')  { seat(state, event.player, (event.player.seed && event.player.seed.bankroll) || 1000); return state; }
  if (event.type === '@leave') { markGone(state, event.player.id); return state; }
  if (event.type === 'play') {
    if (event._by !== ctx.players[state.turn % ctx.players.length].id) ctx.reject('not your turn');
    state.moves.push({ by: event._by, cell: event.cell }); state.turn++; return state;
  }
  if (event.type === '@timer' && event.name === 'settle') {
    for (const p of ctx.players) {
      const net = payout(state, p);
      ctx.update('players', { bankroll: { '$increment': net } }, { id: p.uid });
      ctx.insert('hand_history', { player_id: p.uid, net: net, at: ctx.now() });
    }
    return beginRound(state, ctx);
  }
  return state;
}
```

**Key `state.players` on `player.id`, NEVER `player.uid`.** `player.id` is the client id, always present (even anonymous); `player.uid` is the authed account id and is `undefined` without `requires_auth`, so keying the roster on it collapses every player onto `state.players[undefined]` - "2 online" for one player, everyone else invisible (id-vs-uid: frames contract). Reserve `player.uid` for durable per-account writes (settlement below), which need `requires_auth`.

**Settlement is server-side; the reducer owns the durable state, NOT the client.** Never bridge money/score/history through a client POST - it is cheatable. Patches also take `{'$decrement': n}`; a plain value sets the column. Settle on a round-boundary event (the `@timer` branch above), never on `@tick` - writes declared during `@tick` (`ctx.insert/update/upsert`) are DROPPED (`set_reducer` warns when its probe run sees one), and the tick's state is not persisted - keying durable writes on `player.uid` (id-vs-uid: frames contract; durable settlement needs `requires_auth`). Writes to `of_*` tables or PASSWORD/ENCRYPTED columns are refused.

**Keyed cell/grid/voxel state goes through `ctx.upsert` (contract above), and the key column(s) MUST back a UNIQUE (or PRIMARY KEY) constraint** - add it at `create_table` time (a composite unique on `(x,y,z)`, or `pos TEXT UNIQUE`) or SQLite rejects the ON CONFLICT.

**Seeding from the DB at join** (the `seat(...)` line above): so starting balances are the DB's truth, not client `meta`, declare `seed_table`, `seed_columns` (e.g. `["bankroll"]`), optional `seed_key_column` (default `id`, matched to the authed id; requires `requires_auth`) on `set_reducer`. `@join` then carries `event.player.seed = {bankroll: <db value>}` for a known player, or no `seed` for a new one (apply your default; the first `ctx.insert`/`ctx.upsert` creates the row).

**Cross-room / global state.** A leaderboard / global tally is a table the reducer writes on a round boundary (`ctx.upsert('leaderboard', {user_id: p.uid, wins: {'$increment': 1}}, ['user_id'])`), exposed via a `create_api` / view endpoint - never one giant world-state blob across rooms. To READ global state mid-reduce, declare `shared_table` (+ optional `shared_columns`) on `set_reducer`: every non-`@tick` event then carries `event.shared` = that small, capped table's rows. Authority stays per-room; `event.shared` is read-only context.

**Reconnects:** make `@join` idempotent (a returning `event.player.id` reclaims its slot, `connected:true`) and on `@leave` mark `connected:false` instead of freeing the slot (`markGone` above) - a refreshing player resumes instead of appearing disconnected. Never generate your own per-join id.

Client wiring is `net.onState` + `net.action` (shapes and the never-mutate-locally rule: frames contract). Keep every player's secret in `state` and reveal only their slice in `view(state, playerId)`; do NOT also persist or broadcast game state by hand - the actor does it.

The reducer is run through `@init`/`@join`/`@tick`/`@timer`/`view` at attach time (`set_reducer` refuses one that throws, naming the event and line) AND again at build, so a sandbox violation (contract above), a throw, or a timeout never ships. `set_reducer` also hands rooms that are live right now the new code (they restart from `@init` and replay the roster). To CHANGE or FIX it, fetch the current source with `manage_authoritative action=get endpoint_path=<p>`, edit the exact line, re-attach with `set_reducer` - do not rewrite it blind. Re-attaching REWRITES the `seed_*` and `shared_*` settings from that call's args, so re-send `seed_table`/`seed_columns`/`seed_key_column`/`shared_table`/`shared_columns` every time; omitting them clears them (the result carries only a `warning`). (`manage_realtime` is the model-agnostic manager for ANY realtime endpoint - `list`, `get` its config, or `delete` it, regardless of which model tool created it.)

For a continuously-simulated game, ALSO set `tick_hz`: the actor delivers `{type:'@tick', dt}` to `reduce` that many times a second so you advance the sim server-side (clients still send input via `net.action`). Heaviest mode (a state push per tick per player) - small player counts only.

## Matchmaking
> when: "find me an opponent" / auto-pairing players into a shared room (prefer the server-side pairer).

For "find me an opponent / auto-match", create a `manage_matchmaking` endpoint (`group_size`, default 2) instead of writing pairing logic in client JS. Clients connect to the lobby room and wait; once `group_size` unmatched peers are present the SERVER mints ONE shared match room, assigns roles, and sends each peer:

`{_type:"matched", _room:"m-<random id>", room_field, role:"host"|"player", index:I, players:[ids]}` - the match room is capped at `group_size` (anyone else who learns the id joins as a spectator) (`room_field` is the `room_column` name to switch with; the `ofnet` helper reads it, raw clients send `{ [m.room_field]: m._room }`)

The `ofnet` helper (platform-managed `js/ofnet.js` - do not edit, recreate, or delete it) seeds presence and has ALREADY switched you into `m._room` when `net.onMatched((m) => startGame(m.role, m.players))` fires. Do not hand-roll the switch: server-owned room ids and roles mean peers cannot land in different rooms or both-challenge into a deadlock. `GET /api/<endpoint>/rooms` (-> `[{room,count}]`, `?room=<name>` for one room's client list) shows open games; don't build your own presence-listing protocol.

**Skill / region pairing (optional).** Set `match_by` (`rating`, `region`, or `rating+region`) and carry the attributes in presence meta: `connect("race", { room: "lobby", meta: { rating: 1200, region: "eu" } })`. The server anchors on the longest-waiting peer within a `rating_band` that **widens** with wait (`band_widen_per_sec`), matching any rating after `max_wait_secs`. Both default to 0 (fixed band, no forced match), so set both (e.g. 50 and 30) with `rating`. Tune those three knobs on `manage_matchmaking`; the client only supplies `meta`.

**Matchmaking and an authoritative reducer can NOT share ONE endpoint** - the room actor disables the in-band room switch matched peers rely on, so `set_reducer` on a `manage_matchmaking` endpoint is REFUSED at config time. Pair on the matchmaking endpoint, then connect each matched peer to a separate `manage_authoritative` endpoint (create it with `ephemeral=true` - per-match state, dropped when the room empties - and `max_clients` = `group_size`) with the assigned id:

```js
connect("lobby", { room: "lobby" }).onMatched((m) => {
  const game = connect("game", { room: m._room });   // same identity on both endpoints
  game.onState(render);
  game.action("ready", {});
});
```

The patterns below are only for CUSTOM lobby flows where server matchmaking doesn't fit.

## Custom lobby handoff
> when: a hand-rolled lobby flow where a manage_matchmaking endpoint does not fit (both peers MUST land on ONE shared room id).

Strangers gather in a lobby (`?room=lobby`), pair off, move to a private room. Two traps, both fatal:

- **Each peer minting its OWN match id** (`Date.now()`/`Math.random()`/`crypto.randomUUID()`) - the two land in DIFFERENT rooms and never meet. Reach the SAME id: derive it from the pair (`"match-" + [myClientId, peerId].sort().join("-")`), or the host mints it ONCE and the challenger adopts `msg.room` from the ack.
- **No deterministic initiator** - two peers challenge each other at once, each marks itself "busy", both reject: "still searching" forever. A tiebreaker makes EXACTLY ONE side send the request: `const iAmInitiator = myClientId > peerId;`. Read presence from `net.onPeers` - never invent a parallel announce frame.

Switch rooms over the SAME socket with `room_column`; if you navigate to a separate page, reconnect with the shared id in `?room=`. On a `tick_hz>0` endpoint, send handshake / `finish` frames with `net.event` (or `net.to`): a `net.send` broadcast is coalesced into the snapshot, so a branch waiting for `type:"finish"` never fires.

## Room capacity and friend codes
> when: capping a room, read-only spectators, or private play-with-friends codes.

- **Capacity:** `max_clients=N` caps participants per room, enforced atomically on join; the (N+1)th peer is admitted as a read-only spectator (`welcome._role:"spectator"`, or `net.spectator === true`) whose sends are dropped - gate your input UI on that. A capped room's `welcome` also carries `_capacity`, so render "R/N players" from `_clients`/`_capacity`. Never enforce a player cap in client JS - two simultaneous joins slip past it.
- **Friend codes:** a private room is just a short room name - no `room_gated` token needed when the code is the invite. `roomCode()` (from `'ofnet'`) generates a shareable code; creator and friends both `connect(path, {room: code})`. The relay auto-creates the room on first join; `GET /api/<path>/rooms` answers "is this code live?".
- **Page wiring (home -> game): every navigation (create, join, AND the invite link) must target the SAME route the game's JS is attached to.** The classic dead-on-arrival bug: create/join navigates to `"/?room=" + code` (the HOME route) but the game JS lives at `/game`, so clicking Create just reloads home. Pick ONE game route for all three (`/game?room=<code>`), or one route rendering home-vs-game off `?room=`.

## Server-side cleanup
> when: nothing should persist after a room or session ends.

Do NOT write a `beforeunload`/`sendBeacon` handler or client DELETEs: `sendBeacon` is POST-only, unload deletes are unreliable, and `manage_data` is a BUILD tool absent in the running browser. On a relay/tick endpoint, set `ephemeral:true` and persist any history through its `write_to_table` - when the room empties the relay deletes that room's rows. On an authoritative endpoint, `ephemeral:true` drops the room's reducer state when it empties.

## Audio/video calls over this relay
> when: the app places getUserMedia audio/video calls (also recall webrtc_signaling).

If the project places `getUserMedia` + `RTCPeerConnection` calls, the SDP/ICE handshake rides this same relay, but negotiation, ICE-buffering, TURN, and call-teardown are their own contract - recall `webrtc_signaling` before writing the call client. A text/state-sync app needs none of it.
