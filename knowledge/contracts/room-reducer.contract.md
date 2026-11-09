---
id: room-reducer
title: Authoritative room reducer contract
kind: execution-model
governs: [realtime_signaling]
probes: [ctx.random, ctx.after]
---
**The room reducer (`manage_authoritative set_reducer`).** `reduce(state, event, ctx)` is the only thing that changes an authoritative room's state. The platform runs it in a per-room actor, one event at a time, so it needs no locks. The event shapes it receives are in the realtime-frames contract; this is how the function itself must behave.

**Return the new state on EVERY branch.** `reduce` is pure: its return value IS the next state. A branch that falls through without returning sets the state to `undefined` and empties the room. `state` is `undefined` on `@init` only; that branch builds the initial state.

**It is sandboxed, and the sandbox throws rather than lying to you.** No DOM, no network, no imports, and no ambient nondeterminism:

- `Math.random()` **throws** -> use `ctx.random()` (seeded, deterministic; for shuffles and dice).
- `Date.now()` and a zero-arg `new Date()` **throw** -> use `ctx.now()`. An explicit `new Date(ms)` still works.

These throw because a room has to replay identically to stay authoritative. This is the single most common way a reducer ships broken: it is legal JavaScript everywhere else.

**`ctx` is a PARAMETER of `reduce`, not a global, so THREAD it.** `ctx.random()`/`ctx.now()` read like drop-in swaps for the `Math.random()`/`Date.now()` globals they replace, but `ctx` is the third argument the platform hands `reduce(state, event, ctx)`, and it does not exist anywhere that argument isn't in scope. A helper you factor out of `reduce` (a `resetBall`, a `startRound`) that calls `ctx.random()` without taking `ctx` as a parameter throws `ctx is not defined` the instant it runs, and a throw freezes the room. Pass it in and pass it at every call site: `resetBall(state, ctx, dir)`, not `resetBall(state, dir)`. The same holds for `state` and `event`: a helper reads them only because it was handed them.

**A throw FREEZES the room, so never rely on one to signal a refusal.** When `reduce` throws, the state does not commit, the event is dropped, and the room stays exactly where it was. An event that throws every time it runs (a bad branch reached on every `@tick`) freezes the room permanently while every client keeps sending. Every client is sent `{_type:'error', _message, _fault:true}` and the browser console shows it. To refuse ONE player's action deliberately, call `ctx.reject(msg)`: that is a clean refusal, reported only to the sender, and it leaves the room running.

**`ctx.after(name, seconds)` fires ONCE.** It schedules a single `{type:'@timer', name}` event, server-side, and does not repeat. It survives a client leaving, but the room stops when the LAST client leaves (or the server restarts) and drops pending timers: keep a deadline that must outlive that in state (`state.endsAt`) and settle or re-arm it on `@join`. A countdown re-arms it from its own `@timer` branch, or simply counts down in `@tick` using `dt`:

```js
if (event.type === '@tick' && state.phase === 'countdown') {
  state.left -= event.dt;                                  // dt is seconds
  if (state.left <= 0) { state.phase = 'play'; }
  return state;
}
```

`ctx.cancel(name)` cancels a pending one.

**`ctx` in full.** `random()`, `now()`, `players` (`[{id, uid, meta}]`, the identities are defined in the realtime-frames contract), `reject(msg)`, `after(name, seconds)` / `cancel(name)`, and the durable writes `insert(table, row)` / `update(table, patch, where)` / `upsert(table, row, conflictColumns)`; an `update` patch or `upsert` row value may be a counter `{"$increment": n}` / `{"$decrement": n}` (an upserted row starts at the delta, an existing one adds it). Use `upsert` for keyed cell/grid/voxel state: `update` on a row that does not exist yet silently no-ops and drops every brand-new edit.

**Every write argument is an OBJECT, and `where` is equality matches, never a SQL string.** The reducer holds no DB handle and writes no SQL: `row`/`patch` are plain objects, `where` is `{ id: 7 }` or `{ user_id: uid, room: r }` (keys AND together), and `conflictColumns` is an array of names. A string like `'id = ' + id` throws a TypeError, and a throw here freezes the room.

**Writes and timers are DECLARATIONS, not side effects.** `ctx.insert/update/upsert` and `ctx.after` record an intent; the actor performs it only after the reduce returns successfully. So a reduce that throws performs neither, and the reducer stays pure. A write declared on `@tick` lands too: declare it on the settling tick only.

**Limits, each of which fails that one event rather than the room:** 50ms per call, 256KB of state, 32 declared writes per reduce.

**`view(state, playerId)` is OPTIONAL:** return only what THIS player may see (hidden info). Omit it and everyone sees the whole state. Once you define one it FAILS CLOSED: a `view` that throws or runs over budget sends that player NO state frame and an `{_type:'error', _fault:true}` instead, because the alternative is shipping the hidden state it exists to withhold. So a player whose screen stops updating with a fault in the console is a bug in `view`, not in the client.
