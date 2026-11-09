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

Phaser is a `kind:"classic"` UMD library defining the `window.Phaser` global. Pin the
3.x line - Phaser 4 is a different renderer with a different API surface, and the API
you know is v3. Never substitute a release remembered from elsewhere - this is the
platform's verified pin:

```json
{"name":"phaser", "kind":"classic", "global_name":"Phaser", "url":"https://cdn.jsdelivr.net/npm/phaser@3.90.0/dist/phaser.min.js"}
```

`global_name` is mandatory for every classic library (the undefined-reference gate
keys on it). Arcade physics ships inside this build - never add a separate physics
library for a 2D game.

## Boot inside a fragment-injected page (platform-specific)

Page bodies here are HTML fragments injected into the layout, and your linked module
runs at end-of-body. Never pass a parent id string you hope exists - create the mount
yourself, then hand the element to the config:

```js
const mount = document.getElementById('game');   // the <div id="game"> your page fragment declares (never a second root on body)
const game = new Phaser.Game({
  type: Phaser.AUTO,
  parent: mount,
  backgroundColor: '#1a1c2c',
  scale: { mode: Phaser.Scale.FIT, autoCenter: Phaser.Scale.CENTER_BOTH, width: 960, height: 540 },
  physics: { default: 'arcade', arcade: { gravity: { y: 900 } } },
  scene: [BootScene, PlayScene, UIScene],
});
```

`Scale.FIT` + `autoCenter` is the responsive default: the canvas letterboxes into
whatever space the page gives it, desktop and phone alike. Top-down games set
`gravity: { y: 0 }`.

## Generate textures at runtime

`manage_files` writes text only, so the build cannot author a PNG or spritesheet; a
`this.load.image` of a file that was never saved 404s and Phaser renders a green placeholder
square. (A generated or stock image that manage_image_gen / manage_stockphotos saved under
`/files/` does load.) All art is
generated in a Boot scene, then used by key everywhere else:

```js
class BootScene extends Phaser.Scene {
  create() {
    const g = this.add.graphics();
    g.fillStyle(0xffd166).fillRoundedRect(0, 0, 28, 36, 6);   // draw the hero
    g.fillStyle(0x1a1c2c).fillRect(6, 10, 5, 5).fillRect(17, 10, 5, 5); // eyes
    g.generateTexture('hero', 28, 36);                        // -> usable as 'hero'
    g.clear().fillStyle(0x06d6a0).fillRect(0, 0, 48, 16).generateTexture('platform', 48, 16);
    g.destroy();
    this.scene.start('play');
  }
}
```

This is the platform's sprite pipeline: rects, circles, triangles and rounded shapes
composed per texture, tinted (`setTint`) and scaled for variety. Text and emoji
(`this.add.text(x, y, '🌵', { fontSize: '32px' })`) are legitimate sprites too.
Realise the plan's art direction with these tools - palette, silhouette and motion
carry the look, not asset fidelity.

## Field traps (silent in every gate, only visible in play)

- **Move a physics body with velocity, never `x +=`.** A direct coordinate write
  teleports the body and tunnels straight through colliders.
- **Camera follow needs BOTH world and camera bounds set**, or the view never
  scrolls (or shows void past the edge): `this.physics.world.setBounds(...)` AND
  `this.cameras.main.setBounds(...).startFollow(...)`.

## Particles changed in 3.60 - the old API throws

Models trained on older Phaser write `this.add.particles('tex').createEmitter({...})`.
That API is GONE in 3.60+: `createEmitter` is not a function and the game dies at the
first juice call. The current shape creates the emitter directly:

```js
const burst = this.add.particles(0, 0, 'spark', {
  speed: { min: 60, max: 160 }, lifespan: 400, scale: { start: 1, end: 0 }, emitting: false,
});
burst.explode(12, x, y);   // one burst at the impact point
```

Juice per the game_feel guide rides built-ins: `this.tweens.add` for squash/stretch
and score pops, `this.cameras.main.shake(80, 0.004)` / `.flash(60)` on impact - and
sound is synthesized with raw WebAudio (Phaser's sound manager needs loaded audio
files, which this platform does not have).

<!-- extended -->

## Touch and mobile
> when: the game must play on phones - virtual controls, pointer-driven movement.

Pointer input works untranslated (`this.input.on('pointerdown', ...)`; a pointer is
finger or mouse). For a platformer add on-canvas virtual buttons: semi-transparent
generated-texture sprites fixed to the camera (`setScrollFactor(0)`), each
`setInteractive()` with `pointerdown`/`pointerup` toggling the same flags the
keyboard sets. Test with the smaller FIT viewport in mind: 44px+ touch targets,
controls in the bottom corners.

## Pause, focus and timing
> when: wiring pause menus or debugging "the game jumps after tab-switch".

Phaser auto-pauses its loop on a hidden tab and resumes on focus; `update` deltas
stay sane, so no manual clamp is needed. An explicit pause is
`this.scene.pause('play')` plus an overlay scene; `this.time.addEvent` timers pause
with their scene, `setTimeout` does not - use scene timers for anything gameplay.

## Matter physics
> when: the brief genuinely needs angular/soft-body 2D physics - stacking, seesaws, ragdolls, wrecking balls.

The full Phaser build bundles Matter.js: set `physics: { default: 'matter' }` and
bodies gain rotation, friction, constraints and compound shapes. Arcade stays the
default for platformers and top-down movement (simpler, faster, AABB-only); switch
only when rotation-driven dynamics ARE the game.
