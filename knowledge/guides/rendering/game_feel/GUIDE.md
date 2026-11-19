---
slug: game_feel
role: capability
title: Game feel & juice (canvas / arcade games)
summary: The feel a game owes beyond its mechanics - juice on every hit, score, and death (visible AND audible), a HUD designed as real UI, and no full-screen strobe. Layers game-specific feedback on top of the visual_craft identity rule; pick it for any game or interactive canvas toy.
category: rendering
position: 33
requires_any_of: [phaser_2d, 3d_rendering, voxel_worlds, heightfield_terrain, physics_3d, physics_2d, decision:rendering approach/canvas]
---

# Game feel & juice

Builds on [[visual_craft]] for games and interactive canvas toys:

{{contract:game-runtime}}

- **Visible + audible feedback on every action:** pair hits, scores, and deaths with particles, directional shake, hit-stop, eased tweens, and WebAudio synthesis (no full-screen strobe).
- **2D / platformer craft:** pre-render procedural sprites to offscreen canvases with outline/highlight/shadow; include coyote time (~100 ms), jump buffer (~120 ms), and variable jump height for platformers.

## Choose: the game's visual direction

Derive the game's look from its premise and record it in the plan's `design` and task `intent`: medium (ink, cutout, clay, phosphor, chrome), lighting/time of day, a 3–5 colour palette + accent, and motion character (snappy, floaty, heavy).
