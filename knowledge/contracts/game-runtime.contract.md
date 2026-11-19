---
id: game-runtime
title: Game runtime, assets, and testable HUD
kind: prose
governs: [game_feel]
probes: [manage_files, pointer-events]
---
**Game runtime & testability invariants (any 2D/3D/canvas game).**

- **Text-only assets:** `manage_files` writes text only (no `.png`, `.glb`, or `.mp3` binary files). Generate sprites/textures/models procedurally (offscreen `<canvas>`, inline SVG, `THREE.Group` primitives) and sound with WebAudio synthesis, or use an available image/stock tool if one is in your toolset.
- **DOM HUD & `of_test.js` testability:** Render score, lives, phase (title/play/paused/gameover), and Start/Retry buttons as real DOM elements with stable `id`s inside a `pointer-events: none` overlay (restoring `pointer-events: auto` on interactive buttons/touch controls) rather than painting UI text solely onto `<canvas>`/`WebGL`. Keep the HUD legible on narrow viewports and testable by `of_test.js` without `window.*` globals.
- **Fixed-timestep & `dt` clamp:** Clamp frame `dt <= 0.05` (and step physics on a fixed `1/60`s accumulator) so tab-switching never explodes integrators or camera motion.
