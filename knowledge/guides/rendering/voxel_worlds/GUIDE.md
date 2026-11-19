---
slug: voxel_worlds
role: capability
title: Voxel / block worlds
summary: Minecraft-style world of DISCRETE breakable cubes snapped to an integer grid - per-chunk meshing, ray-targeted break/place, edit persistence, AABB fall-through-floor / step-up physics. Pick ONLY for a genuinely blocky world; NOT smooth/organic/planetary/deformable terrain, even when the brief says "voxel" (that is heightfield_terrain).
use_when: The world is genuinely BLOCKY - discrete cubes on an integer grid that the player breaks and places, Minecraft-style.
not_when: Smooth, organic, planetary or merely deformable terrain, even when the brief calls it "voxel". Diggable ground on its own is not a cube world (that is heightfield_terrain).
category: rendering
position: 31
decisions: [terrain representation/voxel, terrain representation/blocky voxel, terrain representation/blocky]
exclusive_with: [heightfield_terrain]
requires: [3d_rendering]
---

# Voxel / block worlds (Minecraft-style chunked terrain)

Builds on the **3d_rendering** guide (auto-loaded with it). Use this guide only for **blocky** worlds (discrete cubes on an integer grid); smooth/organic terrain belongs to **`heightfield_terrain`**, even when the brief says "voxel".

## Chunk meshing & ray targeting

- **Mesh per chunk, not per block** (`BufferGeometry` per chunk, skipping interior faces between solid neighbours). On any block edit (local or remote), re-mesh the edited chunk **and** any ±x/±z neighbour chunk when the edited cell sits on a chunk boundary (`lx === 0 || lx === CHUNK - 1`, same for `lz`).
- **Per-face vertex base & winding:** Take `const base = positions.length / 3` *per face* right before pushing its 4 corners, wind CCW as seen from outside (`THREE.FrontSide`), and separate transparent blocks (glass, leaves, water) into a second `{ transparent: true, depthWrite: false }` mesh. Derive atlas tile count from the array length (`TILES.length`), never a hardcoded literal.
- **Aim the targeting ray from the camera's rotation**, never hand-written vertical trig (`dy = ±sin(pitch)`):
  ```js
  const dir = new THREE.Vector3(0, 0, -1).applyEuler(new THREE.Euler(pitch, yaw, 0, 'YXZ'));
  ```

## Persisting edits (platform REST upsert)

Generate base terrain deterministically from a shared seed; persist **only player edits** (`{x, y, z, pos, block_type}`, with `block_type: 'air'` for removed blocks).

- Add a `pos TEXT` column (`${x},${y},${z}`) with a **UNIQUE index on `pos`** (composite indexes cannot be addressed by `lookup_column`), and create the endpoint with `lookup_column: "pos"` and all columns writable.
- Upsert edits with `PUT /api/world-edits/${encodeURIComponent(pos)}` (never `POST`, which 400s on duplicate coordinates). Do **not** set `owner_column`/`owner_scope` — otherwise a second player editing a cell gets `404`.
- In multiplayer, also broadcast each edit over the realtime relay for live sync; `PUT` stays the durable store for late-joiners.

<!-- extended -->

## Player physics: spawn, gravity, and 1-block step-up
> when: adding player movement / gravity / collision to the voxel world.

- **Gate gravity on `world.ready`:** Do not run gravity/collision until the spawn chunk is generated, and spawn at `terrainHeight(spawnX, spawnZ) + 1` (in the same world coordinate space `isSolid` uses).
- **Keep player half-width ≤ 0.3** so the AABB doesn't snag on 1-block gaps, and retry blocked horizontal moves 1 block higher (`step-up`):
  ```js
  function moveAxisH(axis, d) {
    pos[axis] += d;
    if (!hitsSolid(pos)) return;
    pos.y += 1;
    if (!hitsSolid(pos)) return;
    pos.y -= 1; pos[axis] -= d;
  }
  ```
