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

Builds on the **3d_rendering** guide (auto-loaded with it). Reach for THIS guide only when the world is genuinely **blocky** - a grid of discrete cubes the player breaks and places, snapped to the integer grid; smooth/organic terrain belongs to **`heightfield_terrain`**, even when the brief says "voxel".

Mesh per chunk, not per cube (one `THREE.Mesh` per block tanks the frame rate): build each chunk's visible faces into ONE `BufferGeometry`, skip interior faces between solid neighbours, and on a block edit re-mesh the chunk that changed AND any ±x/±z neighbour chunk across a face the edit exposed or hid. A break or place on the chunk seam changes the visible faces of the block in the ADJACENT chunk, so re-meshing only the edited chunk leaves that neighbour a stale seam: a missing face you see straight through (or a leftover face floating in air). Re-mesh the neighbour whenever the edited cell sits on a chunk boundary (`lx === 0 || lx === CHUNK - 1`, same for `lz`) - and this holds for edits arriving from OTHER players too, not just local ones, or their builds punch holes in your world at the seams.

**Build each face's triangles off its OWN four vertices, and wind them to face out - the chunk mesher is where "see-through blocks" is born, and no load probe catches it.** A hand-rolled `BufferGeometry` renders, throws nothing, and passes `test_page` while every block is missing faces. Three traps, all invisible until you actually look at the world:

- **Take the vertex base index PER FACE, right before you push that face's 4 corners - never once per block.** Each drawn face appends 4 new vertices, and its two triangles must index THOSE. Hoist the base out to the block (or the chunk) and every face after the first re-indexes the first face's quad - its own vertices are orphaned, so a block shows just ONE of its up-to-six faces and you see straight through it from every other side.

```js
for (const face of FACES) {                     // the 6 unit-cube faces
  const [dx, dy, dz] = face.dir;
  if (isSolid(x+dx, y+dy, z+dz)) continue;       // solid neighbour hides this face - skip it
  const base = positions.length / 3;             // <- PER FACE: indexes the 4 verts pushed next
  for (const c of face.corners) {                // corners wound CCW seen from OUTSIDE the cube
    positions.push(x+c[0], y+c[1], z+c[2]);
    normals.push(...face.normal);
    uv.push(/* atlas tile for this block + face */);
  }
  indices.push(base, base+1, base+2,  base, base+2, base+3);   // two tris off THIS face
}
```

- **Wind every face counter-clockwise as seen from OUTSIDE, or the default `side: THREE.FrontSide` culls the face you want and keeps the one behind it - the world is see-through from half its angles.** If you can't guarantee the winding across all six faces, `side: THREE.DoubleSide` is the escape hatch (costs fill rate); correct winding is better.
- **Give transparent block types (glass, leaves, water) a SECOND geometry + material - a low-alpha tile under `alphaTest` vanishes.** A tile painted at alpha ~0.25 under `{ transparent:true, alphaTest:0.5 }` fails the cutoff on every pixel and the block disappears. Keep opaque blocks in one opaque-material mesh, put see-through types in their own `{ transparent:true, depthWrite:false }` mesh, and don't cull the face between two DIFFERENT transparent types (glass against water) or the seam goes invisible.
- **Divide UVs by the atlas's REAL tile count, not a round number - a wrong divisor makes blocks sample the neighbouring tile, and a block that drifts onto a transparent tile goes see-through.** This is the fourth see-through source, born in the UVs rather than the geometry above: a hardcoded `TILE_N = 16` over a 15-tile atlas shifts every tile ~6% into the next, and the block whose UV lands on a glass or leaf tile (painted with alpha-0 pixels) has those fragments cut by `alphaTest` - a solid-looking block you can see straight through. Derive tiles-across from the atlas itself (`const TILE_N = TILES.length`), never a literal, and inset each tile's UV rect by half a texel (`0.5 / atlasWidthPx`) so `NearestFilter` neighbours don't bleed at the seams (keep mipmaps off).

**Aim the ray along the camera's look direction - derive it, don't hand-trig the sign.** The targeting ray MUST point where the camera actually looks. Re-deriving the pitched forward by hand is the trap: one flipped component - `dy = -sin(pitch)` instead of `+sin(pitch)` - shoots the ray opposite your vertical aim, so you look DOWN at a block and the ray goes UP. Nothing targets, the highlight never lands, and break/place silently no-op - yet it renders and passes every load probe (the same sign trap as WASD in the 3d_rendering guide). Let the engine build the vector from the SAME yaw/pitch the camera uses, so there is no sign to invert:

```js
// look direction = the camera's YXZ rotation applied to -Z. No hand-written trig.
const dir = new THREE.Vector3(0, 0, -1).applyEuler(new THREE.Euler(pitch, yaw, 0, 'YXZ'));
// (equivalently, AFTER the camera's transform is set for the frame: camera.getWorldDirection(dir))
// DDA-march the grid from the eye along dir: the first solid cell is the block to break;
// step back one cell (target + hitFaceNormal) for where a right-click places.
```

**Persist EDITS, not the world.** A procedurally generated world is a pure function of a shared seed - every client regenerates the same terrain locally, so do NOT seed the whole world into a table (you'll fight the bulk-insert cap and store megabytes of derivable data). Persist only what players CHANGE: one row per edited cell, `{x, y, z, block_type}`, where `block_type:'air'` marks a removed block. On load: generate terrain from the seed, then `GET` the edit rows and apply them on top (`air` clears the cell, anything else sets it). The seed is a constant in your JS (or one config row), not 10,000 block rows.

**Re-editing a cell is the common case, so upsert by position with PUT - never POST.** A generated REST `POST` is a plain INSERT: the second edit at the same coordinate hits the position's unique index and returns a `400`, and there is no POST-upsert option (the `manage_data upsert` action is build-time only - the browser can't call it; an `enable_query` endpoint is SELECT-only). The endpoint DOES upsert on a `PUT` addressed by a single TEXT **`lookup_column`** - it updates the row, or creates it when none exists. So key the table by a derived position string: add a `pos TEXT` column set to `` `${x},${y},${z}` `` with a **UNIQUE index on `pos`** (one column - a 3-column composite index can't be addressed by the single-value lookup), and create the endpoint with `lookup_column:"pos"`. Then every placement or break persists idempotently in one call:

```js
// upsert the edit at this cell - overwrites the row if one already exists there
fetch(`/api/world-edits/${encodeURIComponent(pos)}`, {
  method: 'PUT',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ x, y, z, pos, block_type }),   // block_type:'air' = removed
});
```

`x`, `y`, `z`, `pos` and `block_type` must all be writable columns of the endpoint. Do NOT set `owner_column`/`owner_scope` on this edits endpoint: an owner-scoped upsert only reaches the caller's own rows, so a second player editing a cell the first player owns gets a `404` instead of overwriting it.

(Multiplayer only: also broadcast each edit over the realtime relay for live sync; the PUT remains the durable store for late-joiners.)

<!-- extended -->

## Player physics: don't fall through the floor
> when: adding player movement / gravity / collision to the voxel world - the #1 voxel movement bug.

Player physics is gravity + a swept-AABB check against the voxel grid (`isSolid(x,y,z)`). Two ordering traps make the player drop through the ground on spawn:

- **Gravity must not run before the spawn chunk's blocks exist.** If `generateTerrain()` / `loadServerBlocks()` is async (or runs a frame after the loop starts), the first physics ticks see empty air and the player accelerates straight down. Gate it: keep a `world.ready` flag, set it true only once the spawn column is generated, and skip the gravity/collision step (or hold the player frozen) until then.
- **Spawn ON TOP of the terrain, computed from the same height function.** Set the player's feet to `terrainHeight(spawnX, spawnZ) + 1` (plus the player's eye offset), not a hardcoded `y` that may be inside or below the ground. Use the EXACT height/`isSolid` the collision uses, in the SAME world coordinate space (mixing chunk-local and world coords reads the wrong block and the floor "isn't there").

```js
// per frame, after world.ready:
function updatePlayer(dt) {
  if (!world.ready) return;                      // no terrain yet -> don't apply gravity
  if (!fly) vel.y -= GRAVITY * dt;
  // resolve one axis at a time so you slide along walls instead of sticking
  moveAxis('x', vel.x * dt); moveAxis('z', vel.z * dt); moveAxis('y', vel.y * dt);
}
function moveAxis(axis, d) {
  pos[axis] += d;
  if (hitsSolid(pos)) {                          // AABB of [pos +/- half width, pos+height] vs isSolid
    pos[axis] -= d;
    if (axis === 'y') { if (d < 0) onGround = true; vel.y = 0; }  // landed (d<0) or bonked head
  } else if (axis === 'y' && d < 0) {
    onGround = false;
  }
}
// jump only when grounded: if (key('Space') && onGround) { vel.y = JUMP_SPEED; onGround = false; }
```

`hitsSolid` tests every voxel the player's AABB overlaps (feet, head, and the cells around the body), not just the single block under the centre - a one-cell check lets the player clip corners and sink. A "fall through the floor" that a load-time page probe can't see is almost always one of: gravity before `world.ready`, a spawn `y` below the surface, or `isSolid` reading the wrong coordinate space.

## Walk up 1-block steps, and size the player to fit
> when: the player is walled in / "WASD does nothing" on hilly terrain.

The `moveAxis` above reverts the WHOLE horizontal move on contact, so it cannot climb the 1-block rises procedural terrain is full of; on hilly ground the player is walled in on most sides and movement looks completely dead - even though gravity, mouse-look, and block-editing all still work (none of them go through this collision). For the horizontal axes, retry a blocked move one block higher before giving up:

```js
function moveAxisH(axis, d) {               // x / z only (y still uses the moveAxis above)
  pos[axis] += d;
  if (!hitsSolid(pos)) return;
  pos.y += 1;                               // try stepping up one block...
  if (!hitsSolid(pos)) return;              // ...room above: stay raised, gravity settles us onto the step
  pos.y -= 1; pos[axis] -= d;               // wall taller than one block: revert
}
```

Two sizing traps read as "stuck" the same way. A player **half-width over ~0.3** (wider than ~0.6 of a block once doubled) snags on the terrain to either side and can spawn already wedged inside a neighbouring hill - keep the half-width ≤ ~0.3. And derive spawn from the height the player's WHOLE footprint occupies (or just drop the player in a couple of blocks up and let gravity settle), not `terrainHeight(centreX, centreZ) + 1` alone, which can seat a wide body inside a taller adjacent column. None of this - inverted movement, can't-climb, wedged spawn - is visible to a load-time page probe, so verify movement by actually driving the player (a `test_page` only proves the scene loaded).
