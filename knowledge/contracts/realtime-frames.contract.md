---
id: realtime-frames
title: Realtime system frames and presence seeding
kind: wire-envelope
governs: [realtime_signaling]
probes: [welcome._peers]
---
**Realtime system frames (any `create_websocket` endpoint).** The server emits three frames clients receive but never send:

```js
{ _type: 'welcome', _clientId, _clients, _peers, _role, _room }  // only to the newly-connected client
{ _type: 'join',    _sender, _clients, _room, _role, _meta? }  // to OTHER clients when someone joins
{ _type: 'leave',   _sender, _clients, _room }                // to remaining clients on disconnect
```

`_peers` is the roster of peers already present (`[{id, meta}]`, NOT including you); `_meta` is a joiner's profile, present only when they connected with `?meta=`. `_role` is `"player"` or `"spectator"` (always `"player"` unless the room is capacity-capped, which also puts `_capacity`, the `max_clients` value, on `welcome`); a room-gated endpoint hands the room-claiming peer a `_joinToken` in its `welcome`.

**Presence: use `net.onPeers`.** `net.onPeers(peers => ...)` hands you the WHOLE current roster (`[{id, meta}]`, not including you) on connect AND on every join/leave, so you never rebuild it from the frames above - the client tracks it for you. This is the one reliable way to show who is present: reconstructing presence from `join`/`leave` alone makes a joiner see nobody, because it gets no `join` for peers who were already there. Save `myClientId = net.myId` (= `welcome._clientId`) to tell your own frames from peers'. (The three frames are still delivered if you want them: `net.on('welcome'|'join'|'leave', ...)`.)

**Application messages you send.** Whatever you `net.event({ type: 'X', ... })` is broadcast to the OTHER clients in the room with a server-stamped `_sender` (their `_clientId`) added; you get no echo of your own. The receiver dispatches on the frame's `type` - `net.on('X', fn)` matches `type: 'X'` - so **every application message MUST carry a `type` field, and `type` is the discriminator**; a frame without one (or a peer listening on a name that doesn't match) never fires a handler. Trust `_sender` for "who sent this" (it cannot be spoofed); never trust a `from`/`user`/name field inside the body. `net.to(id, {...})` direct-sends to ONE peer instead of broadcasting (the addressing marker is stripped server-side, so the recipient sees only your own fields plus `_sender`). (`net.event` works the same on a relay or a tick endpoint; `net.send` is only for continuous latest-wins state, below.)

```js
{ type: 'chat', text, _sender }   // a peer's frame as YOU receive it: your fields + server _sender
```

**Tick snapshot frame (`tick_hz>0` endpoints only).** A tick endpoint does NOT relay frames one-by-one: it keeps the latest frame each client sent since the previous tick and, `tick_hz` times/sec, emits ONE coalesced snapshot to every client (you included):

```js
{ _type: 'snapshot', _room, states: { '<clientId>': <that client's latest frame this tick>, ... } }
```

`states` is a DELTA, not the roster: only clients that sent since the last tick (no snapshot when nobody did). Spawn/despawn peers from `net.onPeers`, move them from `states`, skip your own `_clientId`. **Two lanes, one endpoint.** Continuous latest-wins state (positions, cursors, an emote flag riding the state frame) is what `net.send(obj)` is for - it coalesces into the snapshot above. A discrete message every peer must receive in full (chat, an emote, a one-off signal) goes via **`net.event(obj)`**: it is delivered as its own frame and dispatched by `type` (`net.on('chat', fn)`), never folded into the snapshot. So movement (`net.send`) and chat (`net.event`) share ONE endpoint - no separate relay endpoint. (Sending chat with `net.send` instead of `net.event` on a tick endpoint is the classic disappearing-chat bug: it is coalesced away and `net.on('chat')` never fires.)

**Authoritative state frame (`authoritative` endpoints only).** A server-reducer endpoint pushes its own state after every change and on join (late-join sync is automatic); the client renders it via `net.onState`, never mutating game state locally from its own action:

```js
{ _type: 'state', state, rev }   // net.onState((state) => render(state))
```

The reducer receives lifecycle + action events: `{type:'@init'}` (state `undefined`), `{type:'@join', player}`, `{type:'@leave', player}`, `{type:'@timer', name}`, `{type:'@tick', dt}` (tick_hz>0 only; **`dt` is SECONDS since the previous tick**, so `x += v * dt` is the integration), and app actions `{type:'<ACTION>', ...payload, _by, player}` (one per `net.action('<ACTION>', payload)`). **An action's payload is spread at the TOP level of the event**, so `net.action('MOVE', {x, y})` reduces as `{type:'MOVE', x, y, _by, player}` and you read `event.x` - there is **no `event.data` wrapper**, and reaching for one throws, which fails the reduce and stops the room applying input while every client keeps sending. `_by` is the sender's server-stamped client id (= `player.id`). The `player` object carries two DISTINCT identities - do not confuse them:

- `player.id` - the connection's client id, **identical to `welcome._clientId` / the client's `net.myId`**. Always present (incl. anonymous). **Key your roster (`state.players[...]`) on this** so the client can match its own entry (`state.players[net.myId]`) and skip it (`if (pid === net.myId) continue`).
- `player.uid` - the authed numeric account id; present ONLY on `requires_auth:true`, **`undefined` otherwise**. Use for durable per-account `ctx` DB writes, NEVER as a roster key on an anonymous endpoint (every player collapses onto `state.players[undefined]` → "2 online" for one player, peers invisible).
- `player.meta` - the client's `?meta=` profile; `player.seed` - the DB seed row (seed_table), on `@join` only.
