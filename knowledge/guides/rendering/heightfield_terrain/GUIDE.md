---
slug: heightfield_terrain
role: capability
title: Smooth / heightfield terrain (planets, open worlds)
summary: Smooth, organic, DEFORMABLE terrain - No Man's Sky-style planets, open worlds, dunes, flight/driving sims. A continuous heightfield / displacement mesh with noise biomes and dig-to-deform, NOT a grid of cubes. Pick this (not voxel_worlds) whenever terrain is smooth, even when the brief says "voxel".
use_when: Terrain is smooth or organic - a No Man's Sky-style planet, an open-world explorer, dunes, or a flight/driving sim. Also the right pick for diggable or deformable ground that is not made of cubes.
not_when: A world built from discrete breakable cubes snapped to an integer grid (that is voxel_worlds).
category: rendering
position: 32
decisions: [terrain representation/smooth heightfield, terrain representation/heightfield, terrain representation/heightfield terrain, terrain representation/smooth]
exclusive_with: [voxel_worlds]
exclusive_default: true
requires: [3d_rendering]
---

# Smooth / heightfield terrain (planets, open worlds, flight sims)

Builds on the **3d_rendering** guide (auto-loaded with it). Reach for THIS guide when the
world is **smooth and organic** - a No Man's Sky-style planet, an open-world explorer,
rolling hills, dunes, a flight or driving sim: a continuous surface displaced by noise that
the player can *deform* (mine, carve craters, dig tunnels) by editing the surface, not by
removing discrete cubes; a grid of discrete breakable cubes (Minecraft/Terraria) is the
`voxel_worlds` guide, even when the brief calls smooth terrain "voxel".

**Displace from the pinned noise constants.** Displace the surface (a subdivided plane, or
a cube-sphere/icosphere for a planet) by noise seeded from the world seed, using the
amplitude/frequency (and seed) locked in the plan's decision record so BUILD and any later
regen agree and the same seed regenerates the same world. The displacement math itself -
fBm octaves, cube-sphere wrapping, biome colouring from a second noise - is standard
technique; the platform-specific part is keeping the constants pinned so regen is
deterministic.

**Physics samples the SAME height function the mesh was displaced with.** This is the #1
trap and the #1 difference from `voxel_worlds`: there is no `isSolid(x,y,z)` grid. Each
frame, sample the ground under the player (`h = terrainHeight(px, pz)`, or the radial height
for a planet), apply gravity, and clamp the feet up to `h + eyeHeight` when they drop below
it. Use ONE shared `terrainHeight(x,z)` for both mesh and physics - two copies drift and the
player floats above or clips through the ground. Derive the mining ray from
`camera.getWorldDirection`, never hand-written trig (see the 3d_rendering guide).

**Persist edits as sparse deltas - and a shared world's deltas are authoritative SERVER
state, not client localStorage (see the `realtime_signaling` guide).** A hit edits the heightfield
(lower vertices within a radius, then recompute normals for THAT patch only, or the crater
lights flat and looks like paper). Store only the edits (patch id / world position + radius
+ depth), replayed onto the seed-rebuilt base on load: the seed rebuilds the world, the
deltas rebuild the player's scars.

**This is not `voxel_worlds`.** If you find yourself writing `isSolid`, chunk meshing of
cubes, integer-grid block placement, or step-up physics, you are on the wrong guide - that
is a discrete-cube world. Smooth terrain uses displacement + sample-and-clamp.

## Decision: terrain noise constants (record in the plan's `decisions`)

Lock the world's noise constants - seed, amplitude, and frequency, as concrete numbers.
Invent them for THIS project: a seed unique to this world (never a number remembered from
an example or another build), amplitude and frequency sized to the brief's terrain
character - gentle plains sit far from jagged peaks. BUILD displaces the mesh from exactly
these and a regen replays them, so the same seed rebuilds the same world; unpinned
constants make every regen a different planet.
