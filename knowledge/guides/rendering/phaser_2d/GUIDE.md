---
slug: phaser_2d
role: capability
title: 2D games with Phaser
summary: Phaser 3 setup for 2D games (platformer, top-down, arcade, shooter, puzzle) - the pinned UMD build, runtime-generated art (manage_files writes text, so project art is drawn at runtime), scene structure, arcade physics, camera/input wiring, and the post-3.60 particle API. Pick this over three.js when the game is genuinely 2D.
use_when: The game is 2D - a platformer, top-down roamer, side-scroller, arcade shooter, brick-breaker, tower defense, or physics puzzle. Also the right pick when a brief says "retro", "pixel", or "sprite" with no 3D language.
not_when: Anything with a 3D camera, terrain, or first/third-person view (that is 3d_rendering), or a trivial one-interaction canvas toy where a hand-rolled rAF loop is less code than a framework.
category: rendering
position: 31
libraries: [phaser]
decisions: [rendering approach/phaser, rendering engine/phaser, rendering approach/phaser 3]
---

# 2D games with Phaser (platformer, top-down, arcade)

## Choose: the pinned Phaser entry (copy into `external_libraries` EXACTLY)

Phaser is `kind:"classic"` (`window.Phaser`). Pin v3 (Phaser 4 has a different API; `global_name` is mandatory for the undefined-reference gate, and Arcade physics is built in):

```json
{"name":"phaser", "kind":"classic", "global_name":"Phaser", "url":"https://cdn.jsdelivr.net/npm/phaser@3.90.0/dist/phaser.min.js"}
```

## Platform mount & runtime textures

- **Mount into a fragment container:** pages are HTML fragments injected into the layout and linked modules run at end-of-body. Pass `parent: document.getElementById('game')` with `scale: { mode: Phaser.Scale.FIT, autoCenter: Phaser.Scale.CENTER_BOTH, width: 960, height: 540 }`.
- **Generate textures in a Boot scene:** `manage_files` writes text only, so `this.load.image` of an unsaved path 404s (use `manage_image_gen`/`manage_stockphotos` or `this.add.graphics().generateTexture(key, w, h)` / emoji text sprites):
  ```js
  const g = this.add.graphics();
  g.fillStyle(0xffd166).fillRoundedRect(0, 0, 28, 36, 6).generateTexture('hero', 28, 36);
  g.destroy();
  ```
- **Field traps:** move physics bodies with velocity (never `x +=`), set both `this.physics.world.setBounds(...)` and `this.cameras.main.setBounds(...).startFollow(...)`, synthesize sound with WebAudio (no audio asset files), and use the **Phaser 3.60+ particle API** (`createEmitter` was removed):
  ```js
  const burst = this.add.particles(0, 0, 'spark', { speed: { min: 60, max: 160 }, lifespan: 400, scale: { start: 1, end: 0 }, emitting: false });
  burst.explode(12, x, y);
  ```

<!-- extended -->

## Touch, pause, and Matter physics
> when: mobile touch controls, pause/timers, or angular 2D physics.

- **Touch:** add `setScrollFactor(0).setInteractive()` sprites in bottom corners with 44px+ targets.
- **Pause/timers:** use `this.scene.pause('play')` and `this.time.addEvent` (pauses with the scene; `setTimeout` does not).
- **Matter.js:** bundled in this build (`physics: { default: 'matter' }`) — use only when rotation/soft-body/stacking is core; keep Arcade for platformers/top-down.
