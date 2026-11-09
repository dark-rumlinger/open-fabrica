---
slug: physics_3d
role: capability
title: 3D rigid-body physics (cannon-es)
summary: Real rigid-body dynamics for three.js scenes - stacking, knocking over, vehicles, projectiles that bounce and roll, ragdoll-ish tumbling - via cannon-es: the pinned ESM build, the body/mesh pairing rule, fixed-timestep stepping, and when NOT to use a physics engine at all.
use_when: Objects must push, stack, topple, roll, bounce off each other, or swing from constraints - dominoes, wrecking balls, marble runs, physics puzzles, vehicles with suspension.
not_when: Walking or driving on terrain (sample the heightfield and clamp, per the terrain guide), simple projectiles with no bounce, or any 2D game (Phaser's built-in physics covers those).
category: rendering
position: 32
libraries: [cannon]
requires: [3d_rendering]
---

# 3D rigid-body physics with cannon-es

cannon-es is a `kind:"module"` pure-JS library - no WASM, no build step, works from
the CDN ESM URL directly:

```json
{"name":"cannon-es", "kind":"module", "module_name":"cannon-es", "url":"https://cdn.jsdelivr.net/npm/cannon-es@0.20.0/dist/cannon-es.js"}
```

`import * as CANNON from 'cannon-es';` in every file that touches it, same
never-funnel-through-a-state-bag rule as THREE.

**First decide you need it at all.** A character walking on terrain is the terrain
guide's sample-and-clamp, NOT a physics engine - adding one there buys jitter and
tunneling for nothing. A physics engine earns its place when objects interact with
EACH OTHER: stacks that topple, a ball that caroms, a load that swings.

## The one rule: every dynamic object is a body/mesh pair, physics drives graphics

The world simulates CANNON bodies; three.js draws meshes. After each step, copy pose
from body to mesh - never write mesh positions into dynamic bodies (that fights the
solver and explodes the stack):

```js
const world = new CANNON.World({ gravity: new CANNON.Vec3(0, -9.82, 0) });

const box = new CANNON.Body({ mass: 2, shape: new CANNON.Box(new CANNON.Vec3(0.5, 0.5, 0.5)) });
box.position.set(0, 8, 0);
world.addBody(box);
const boxMesh = new THREE.Mesh(boxGeo, boxMat);
scene.add(boxMesh);
pairs.push({ body: box, mesh: boxMesh });   // one registry, one sync loop

function physicsFrame(dt) {
  world.step(1 / 60, Math.min(dt, 0.05), 3);   // fixed 60Hz, clamped dt, max 3 substeps
  for (const { body, mesh } of pairs) {
    mesh.position.copy(body.position);
    mesh.quaternion.copy(body.quaternion);
  }
}
```

`mass: 0` makes a body STATIC (ground, walls) - it never moves and costs nothing.
The clamp on `dt` matters: stepping with a raw tab-wake delta of seconds makes the
solver spiral and launch everything into orbit.

## Shapes and stability: the traps

- **Build collision from primitives** (`Box`, `Sphere`, `Cylinder`) and compounds
  (`body.addShape(shape, offset)` on one body), never a triangle-mesh of the render
  geometry - trimesh collision is slow, surface-only, and tunnels; a rough hull reads
  fine (players read motion, not hulls). Bounce and friction live on a
  `ContactMaterial`, not on the bodies.
- **Work in meters and real-ish masses** - sub-0.1-unit bodies jitter; scale the scene
  up rather than shrinking physics.
- **Remove bodies outside the step** (queue them, flush after `world.step`), never from
  a collision callback mid-step.

<!-- extended -->

## Heightfield ground
> when: dynamic objects must roll and settle on the terrain itself, not on flat ground.

`CANNON.Heightfield` takes the SAME height samples the terrain mesh was displaced
with (one `terrainHeight(x, z)` source, per the terrain guide) on a regular grid with
`elementSize` matching the mesh spacing. Orient it with the terrain (the shape's
local frame expects rows along x, columns along z, heights up) and keep the grid
coarse - physics needs far fewer samples than the render mesh. The player character
STILL walks by sample-and-clamp; the heightfield body exists for the props.

## Constraints
> when: swinging, hinging, or towing - wrecking balls, doors, trailers, rope bridges.

`PointToPointConstraint` pins two bodies at an offset (chains, ropes as segment
chains), `HingeConstraint` gives doors and seesaws an axis, `LockConstraint` welds.
Constraints need a few solver iterations to look rigid: raise
`world.solver.iterations` toward 10-20 when a chain stretches visibly.
