---
slug: physics_2d
role: capability
title: 2D rigid-body physics (Matter.js)
summary: 2D rigid-body dynamics for PixiJS or vanilla canvas games (stacking, ragdolls, pinball, catapults, rotating joints) via Matter.js: the pinned UMD build, fixed-step Engine.update, body/visual sync, and when to use Phaser Arcade or simple AABB instead.
use_when: A 2D PixiJS or vanilla canvas game needs real rigid-body rotation, stacking, friction, restitution, or constraints (pinball, Angry-Birds-style launchers, ragdolls, physics puzzles).
not_when: A Phaser 3 game (Phaser bundles Arcade and Matter — use phaser_2d), a 3D game (use physics_3d), or simple top-down/platformer AABB movement where Matter.js adds unnecessary solver complexity.
category: physics
position: 33
libraries: [matter]
decisions: [physics model/rigid body 2d, physics model/matter, physics model/matter js]
---

# 2D rigid-body physics with Matter.js

## Decision: physics model (record in the plan's `decisions`)

Lock `topic: "physics model"` as `choice: "rigid-body-2d"` when a PixiJS or vanilla `<canvas>` 2D game needs rotating bodies, stacking, or constraints (declare `matter-js` below). If the game uses Phaser 3, use `phaser_2d` (Arcade by default, or Phaser's bundled `matter`) instead of adding standalone `matter-js`. For multiplayer, `Matter.js` runs client-side over `manage_tick` (`realtime_signaling`) because `manage_authoritative` reducers cannot import external libraries.

## Choose: the pinned Matter.js entry (copy into `external_libraries` EXACTLY)

Matter.js is `kind:"classic"` (`window.Matter`; `global_name` is mandatory for the undefined-reference gate):

```json
{"name":"matter-js", "kind":"classic", "global_name":"Matter", "url":"https://cdn.jsdelivr.net/npm/matter-js@0.20.0/build/matter.min.js"}
```

## Fixed-step simulation & body/render sync

Never pass raw `requestAnimationFrame` deltas straight into `Matter.Engine.update` (and never use `Matter.Runner.run` alongside your own `rAF` loop — that double-steps physics). Step on a fixed `1000 / 60` ms accumulator with `dt` clamped to `<= 0.05`s, then copy `body.position` and `body.angle` to your canvas/Pixi sprites:

```js
const engine = Matter.Engine.create();
const FIXED_MS = 1000 / 60;
let acc = 0;

function tick(dtSec) {
  acc += Math.min(dtSec, 0.05) * 1000;
  while (acc >= FIXED_MS) {
    Matter.Engine.update(engine, FIXED_MS);
    acc -= FIXED_MS;
  }
  for (const { body, sprite } of pairs) {
    sprite.position.set(body.position.x, body.position.y);
    sprite.rotation = body.angle;
  }
}
```

- **Static boundaries:** pass `{ isStatic: true }` for walls, floors, and bumpers.
- **Move bodies physically:** use `Matter.Body.setVelocity`, `applyForce`, or `setAngularVelocity` (never mutate `body.position` directly on dynamic bodies, or collisions tunnel).
- **Queue removals:** remove bodies from `Matter.Composite.remove(engine.world, body)` *after* `Engine.update` finishes, never inside `collisionStart` callbacks.

<!-- extended -->

## Constraints & sleeping
> when: catapults, pendulums, ragdolls, or large stacks of resting blocks.

Use `Matter.Constraint.create({ bodyA, bodyB, pointA, pointB, stiffness })` for pivots/slingshots and `Matter.Engine.create({ enableSleeping: true })` so resting stacks settle without jittering.
