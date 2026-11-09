---
slug: game_feel
role: capability
title: Game feel & juice (canvas / arcade games)
summary: The feel a game owes beyond its mechanics - juice on every hit, score, and death (visible AND audible), a HUD designed as real UI, and no full-screen strobe. Layers game-specific feedback on top of the visual_craft identity rule; pick it for any game or interactive canvas toy.
category: rendering
position: 33
requires_any_of: [phaser_2d, 3d_rendering, voxel_worlds, heightfield_terrain, physics_3d, decision:rendering approach/canvas]
---

# Game feel & juice

Commit to one specific look and vary it across projects: that identity rule is [[visual_craft]],
and it covers the canvas as much as a page. This guide adds what a GAME owes on top of it - the
art direction lands ON the canvas, a HUD frames it, and every action answers back.

**Juice every action.** A hit, a score, a death each needs feedback that is visible AND audible
(particles, screen shake, hit-stop, a sound), not a silent number change. The technique is
familiar; what matters is that no action lands without a response. Never full-screen strobe.

**A working vocabulary, by category** - techniques to draw on, never constants to copy:

- Impact: hit-stop for a few frames, decaying directional screen shake, a particle burst
  that inherits the hit's direction, a one-frame flash on the thing struck.
- Motion: squash-and-stretch on jumps and landings, anticipation before a big action,
  eased tweens (never linear) on anything that moves in the UI.
- Reward: score pops that scale with the streak, combos that escalate pitch and size,
  pickups that arc toward the HUD, a celebration for a new best.
- Ambience: idle motion so the world never freezes (drift, sway, blink), parallax.
- Audio: synthesize with WebAudio - short envelopes, a touch of random pitch per repeat;
  a state-change jingle, not only click blips.

**Design the HUD as real UI**, not debug text in a corner: score, lives, timer and state
laid out with the same care as the look. A touch-control layer rides full-screen
with `pointer-events:none`, each stick and button restoring its own `auto`, so the layer
never eats clicks aimed beneath it.

**Design every game state, not just play.** A title screen that sets the mood, a pause that
dims the world, a game-over that lands (score, why it ended, one key to retry), and a
first-run state that teaches by doing. Edge states (nobody here yet, connection lost)
are designed screens, not raw text.

## Choose: the game's visual direction

Choose the game's look - palette, type, and signature motion - and record it in the plan's
`design` and the game task's `intent`. Derive it from the game's premise, mood and audience
first, then pick from the full space, not the default corner:
low-poly pastoral (green hills, pastel village) is the look every unprompted game ships, so
reach for it only when the brief asks for it. Decide along a few axes and combine them in a
way this premise earns: the medium it imitates (ink, cutout, clay, phosphor, chrome), the
light (time of day, one dominant color, hard or soft), a palette of 3 to 5 colors with one
accent, and the motion character (snappy, floaty, heavy). BUILD realises the world those
words describe, the way `visual_craft` asks of any project; a plan without them ships the
default look every other game already has.

## Canvas 2D craft (vanilla canvas or Phaser)

A game of flat rectangles reads as a prototype, and the way past it needs no image files:
- Draw each sprite from paths and gradients once into an offscreen canvas (or a generated
  texture) with an outline, highlight and shadow, then blit it, so actors read apart at once.
- Layer the scene: gradient sky, two or three parallax bands, ground detail, then actors,
  with a smoothed follow camera rather than a hard lock.
- Step physics on a fixed timestep so feel is the same at 60 and 144 Hz, and keep jump,
  speed and friction as named constants.
- A platformer forgives the player: coyote time (~100 ms to jump after leaving a ledge), a
  jump buffer (~120 ms for an early press) and variable height (cut the jump on release).
